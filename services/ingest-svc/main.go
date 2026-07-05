package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/cmar82/dapr-stuff/services/shared/finops"
	daprd "github.com/dapr/go-sdk/client"
)

const (
	stateStoreCostCenters = "state-redis"    // hot lookup — Redis
	stateStoreLineItems   = "state-postgres" // durable, queryable
	pubsubName            = "pubsub-rabbitmq"
	topicName             = "line-item.enriched"
	seedFilePath          = "/seed/cost-centers.json"
)

// stats is a lock-free counter set for /stats.
type stats struct {
	Received  atomic.Int64
	Enriched  atomic.Int64
	Published atomic.Int64
	Failed    atomic.Int64
	Unmapped  atomic.Int64
}

func main() {
	ctx := context.Background()

	dapr, err := daprd.NewClient()
	if err != nil {
		log.Fatalf("dapr client: %v", err)
	}
	defer dapr.Close()

	// Seed cost-center → team mapping into the state store on startup.
	// Doing this exercises Dapr state.SET in bulk and lets ingest read via
	// state.GET on every enrichment.
	if err := seedCostCenters(ctx, dapr, seedFilePath); err != nil {
		log.Fatalf("seed cost centers: %v", err)
	}

	s := &stats{}
	lookup := makeLookup(ctx, dapr)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/stats", handleStats(s))
	mux.HandleFunc("/ingest", handleIngest(ctx, dapr, lookup, s))

	addr := ":" + envOr("PORT", "8080")
	log.Printf("ingest-svc listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// makeLookup returns a LookupFunc backed by the Dapr state-redis store.
func makeLookup(ctx context.Context, dapr daprd.Client) finops.LookupFunc {
	return func(id string) (finops.CostCenterInfo, bool, error) {
		item, err := dapr.GetState(ctx, stateStoreCostCenters, "cost-center:"+id, nil)
		if err != nil {
			return finops.CostCenterInfo{}, false, fmt.Errorf("dapr state get: %w", err)
		}
		if item == nil || len(item.Value) == 0 {
			return finops.CostCenterInfo{}, false, nil
		}
		var info finops.CostCenterInfo
		if err := json.Unmarshal(item.Value, &info); err != nil {
			return finops.CostCenterInfo{}, false, fmt.Errorf("decode lookup value: %w", err)
		}
		return info, true, nil
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"okay","service":"ingest-svc"}`))
}

func handleStats(s *stats) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"received":  s.Received.Load(),
			"enriched":  s.Enriched.Load(),
			"published": s.Published.Load(),
			"failed":    s.Failed.Load(),
			"unmapped":  s.Unmapped.Load(),
		})
	}
}

// handleIngest accepts NDJSON on POST /ingest and, for each line:
//   - parses to a LineItem
//   - enriches via cost-center lookup (Dapr state.GET on state-redis)
//   - saves the enriched item to state-postgres
//   - publishes to line-item.enriched via pubsub-rabbitmq
//
// Returns a summary of the batch. Unmapped items (no/unknown cost-center tag)
// are counted separately from real failures — they represent unallocated
// cloud spend, not a system error.
func handleIngest(ctx context.Context, dapr daprd.Client, lookup finops.LookupFunc, s *stats) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}

		type summary struct {
			Received  int64 `json:"received"`
			Enriched  int64 `json:"enriched"`
			Published int64 `json:"published"`
			Failed    int64 `json:"failed"`
			Unmapped  int64 `json:"unmapped"`
		}
		batch := summary{}

		scanner := bufio.NewScanner(r.Body)
		scanner.Buffer(make([]byte, 0, 1<<16), 4<<20) // up to 4 MiB per line
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			s.Received.Add(1)
			batch.Received++

			var item finops.LineItem
			if err := json.Unmarshal(line, &item); err != nil {
				log.Printf("parse line: %v", err)
				s.Failed.Add(1)
				batch.Failed++
				continue
			}

			enriched, err := finops.Enrich(item, lookup)
			if err != nil {
				switch err {
				case finops.ErrMissingCostCenter, finops.ErrUnknownCostCenter:
					s.Unmapped.Add(1)
					batch.Unmapped++
				default:
					log.Printf("enrich %s: %v", item.ID, err)
					s.Failed.Add(1)
					batch.Failed++
				}
				continue
			}
			s.Enriched.Add(1)
			batch.Enriched++

			body, _ := json.Marshal(enriched)

			if err := dapr.SaveState(ctx, stateStoreLineItems, "line-item:"+enriched.ID, body, nil); err != nil {
				log.Printf("save line-item %s: %v", enriched.ID, err)
				s.Failed.Add(1)
				batch.Failed++
				continue
			}
			// Publish the struct itself (not the []byte) so the Dapr SDK sets
			// datacontenttype=application/json on the CloudEvent envelope.
			// Passing []byte would tag it as opaque bytes, and the receiver
			// would see `data` as a JSON-encoded string, not an object.
			if err := dapr.PublishEvent(ctx, pubsubName, topicName, enriched); err != nil {
				log.Printf("publish %s: %v", enriched.ID, err)
				s.Failed.Add(1)
				batch.Failed++
				continue
			}
			s.Published.Add(1)
			batch.Published++
		}
		if err := scanner.Err(); err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(batch)
	}
}

func seedCostCenters(ctx context.Context, dapr daprd.Client, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var centers []finops.CostCenterInfo
	if err := json.NewDecoder(f).Decode(&centers); err != nil {
		return fmt.Errorf("decode seed: %w", err)
	}

	log.Printf("seeding %d cost centers into %s", len(centers), stateStoreCostCenters)
	for _, cc := range centers {
		body, _ := json.Marshal(cc)
		if err := dapr.SaveState(ctx, stateStoreCostCenters, "cost-center:"+cc.CostCenterID, body, nil); err != nil {
			return fmt.Errorf("save %s: %w", cc.CostCenterID, err)
		}
	}
	log.Printf("seed complete")
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
