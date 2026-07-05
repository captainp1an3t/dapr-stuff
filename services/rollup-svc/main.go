package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cmar82/dapr-stuff/services/shared/finops"
	daprd "github.com/dapr/go-sdk/client"
)

const (
	stateStore  = "state-postgres" // durable rollups + processed markers
	maxETagRetries = 8
)

// stats is a lock-free counter set for /stats.
type stats struct {
	Received  atomic.Int64
	Applied   atomic.Int64 // rollup update succeeded
	Duplicate atomic.Int64 // seen before, skipped
	BadEnv    atomic.Int64 // bad CloudEvent envelope
	Failed    atomic.Int64 // real errors (state store, retries exhausted)
}

// Dapr CloudEvent envelope. Only the fields we actually consume.
type cloudEvent struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

// Dapr pub/sub response envelope. Returning "SUCCESS" ACKs the message;
// "RETRY" tells Dapr to redeliver; "DROP" ACKs but signals a poison message.
type subResponse struct {
	Status string `json:"status"`
}

var (
	success = mustJSON(subResponse{Status: "SUCCESS"})
	retry   = mustJSON(subResponse{Status: "RETRY"})
	drop    = mustJSON(subResponse{Status: "DROP"})
)

func main() {
	ctx := context.Background()

	dapr, err := daprd.NewClient()
	if err != nil {
		log.Fatalf("dapr client: %v", err)
	}
	defer dapr.Close()

	s := &stats{}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/stats", handleStats(s))
	mux.HandleFunc("/events/line-item-enriched", handleLineItemEnriched(ctx, dapr, s))

	addr := ":" + envOr("PORT", "8080")
	log.Printf("rollup-svc listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"okay","service":"rollup-svc"}`))
}

func handleStats(s *stats) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"received":  s.Received.Load(),
			"applied":   s.Applied.Load(),
			"duplicate": s.Duplicate.Load(),
			"bad_env":   s.BadEnv.Load(),
			"failed":    s.Failed.Load(),
		})
	}
}

// handleLineItemEnriched processes one CloudEvent carrying an enriched line
// item. Semantics:
//   - malformed envelope or payload → DROP (poison message, ACK and skip)
//   - already-seen line item        → SUCCESS (idempotent, ACK)
//   - transient state store error   → RETRY (Dapr redelivers)
//   - rollup upsert exhausts ETag retries → RETRY
//   - success                       → SUCCESS
func handleLineItemEnriched(ctx context.Context, dapr daprd.Client, s *stats) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.Received.Add(1)
		w.Header().Set("Content-Type", "application/json")

		var evt cloudEvent
		if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
			log.Printf("bad envelope: %v", err)
			s.BadEnv.Add(1)
			_, _ = w.Write(drop)
			return
		}

		var item finops.EnrichedLineItem
		if err := json.Unmarshal(evt.Data, &item); err != nil {
			log.Printf("bad payload for evt %s: %v", evt.ID, err)
			s.BadEnv.Add(1)
			_, _ = w.Write(drop)
			return
		}

		// Idempotency: mark the line-item as processed with FirstWrite. If it
		// already exists, another delivery of the same event handled this.
		dup, err := markProcessed(ctx, dapr, item.ID)
		if err != nil {
			log.Printf("processed-mark error for %s: %v", item.ID, err)
			s.Failed.Add(1)
			_, _ = w.Write(retry)
			return
		}
		if dup {
			s.Duplicate.Add(1)
			_, _ = w.Write(success)
			return
		}

		if err := applyRollup(ctx, dapr, item); err != nil {
			log.Printf("apply rollup for %s: %v", item.ID, err)
			s.Failed.Add(1)
			_, _ = w.Write(retry)
			return
		}
		s.Applied.Add(1)
		_, _ = w.Write(success)
	}
}

// markProcessed tries to insert "processed:<id>" with FirstWrite concurrency.
// Returns (dup=true, err=nil) if the key already existed.
func markProcessed(ctx context.Context, dapr daprd.Client, id string) (bool, error) {
	key := "processed:" + id
	err := dapr.SaveState(ctx, stateStore, key, []byte("1"), nil,
		daprd.WithConcurrency(daprd.StateConcurrencyFirstWrite),
		daprd.WithConsistency(daprd.StateConsistencyStrong),
	)
	if err == nil {
		return false, nil
	}
	if isConcurrencyConflict(err) {
		return true, nil
	}
	return false, err
}

// applyRollup upserts the rollup for this line item using ETag-based
// optimistic concurrency to survive races with other subscriber replicas or
// parallel dispatches.
func applyRollup(ctx context.Context, dapr daprd.Client, item finops.EnrichedLineItem) error {
	delta := finops.FromLineItem(item, time.Now())
	key := delta.Key()

	for attempt := 1; attempt <= maxETagRetries; attempt++ {
		got, err := dapr.GetState(ctx, stateStore, key, nil)
		if err != nil {
			return err
		}

		var base finops.Rollup
		if got != nil && len(got.Value) > 0 {
			if err := json.Unmarshal(got.Value, &base); err != nil {
				return err
			}
		}

		merged := base.Merge(delta)
		body, err := json.Marshal(merged)
		if err != nil {
			return err
		}

		// If we read a value with an etag, only overwrite if it hasn't changed
		// under us. If the key didn't exist, SaveState with FirstWrite ensures
		// nobody else raced us to create it.
		if got != nil && got.Etag != "" {
			err = dapr.SaveStateWithETag(ctx, stateStore, key, body, got.Etag, nil,
				daprd.WithConcurrency(daprd.StateConcurrencyLastWrite),
				daprd.WithConsistency(daprd.StateConsistencyStrong),
			)
		} else {
			err = dapr.SaveState(ctx, stateStore, key, body, nil,
				daprd.WithConcurrency(daprd.StateConcurrencyFirstWrite),
				daprd.WithConsistency(daprd.StateConsistencyStrong),
			)
		}
		if err == nil {
			return nil
		}
		if !isConcurrencyConflict(err) {
			return err
		}
		// Backoff a hair; another goroutine won the race, re-read and re-merge.
		time.Sleep(time.Duration(attempt) * 10 * time.Millisecond)
	}
	return errors.New("etag retries exhausted")
}

// isConcurrencyConflict detects Dapr's ETag / FirstWrite conflict errors.
// The wire text varies by state store implementation:
//   - postgres v2: "no item was updated" (INSERT ON CONFLICT DO NOTHING → 0 rows affected)
//   - redis:       "possible etag mismatch"
//   - generic:     "etag mismatch", "already exists", "duplicate key"
func isConcurrencyConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "etag") ||
		strings.Contains(msg, "possible etag mismatch") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "no item was updated")
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
