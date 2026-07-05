CLUSTER_NAME := dapr-stuff
KIND_CONFIG  := cluster/kind-config.yaml
KUBE_CTX     := kind-$(CLUSTER_NAME)
MONITORING_NS := monitoring
DAPR_NS       := dapr-system
CA_EXTRAS    := .ca-extras.pem

.DEFAULT_GOAL := help

## ---- Front-door targets ---------------------------------------------------

.PHONY: up
up: prep cluster-ensure infra-install dapr-install ## Bring up KinD + observability + Dapr + Tilt (blocks until Ctrl-C)
	tilt up

.PHONY: down
down: ## Shut down Tilt and delete the KinD cluster (blows away observability with it)
	-tilt down
	$(MAKE) cluster-delete

.PHONY: verify
verify: ## Smoke-test the current stack (run after `make up` in another shell)
	@echo "== KinD cluster =="
	kubectl --context $(KUBE_CTX) get nodes
	@echo
	@echo "== base images contain the extra CA bundle (byte-exact tail match) =="
	@if [ ! -s $(CA_EXTRAS) ]; then \
		echo "  (skipped — $(CA_EXTRAS) is empty, no extra CAs configured)"; \
	else \
		for img in dapr-stuff/base-builder dapr-stuff/base-runtime; do \
			docker run --rm $$img sh -c ' \
				size=$$(wc -c < /usr/local/share/ca-certificates/extras.crt); \
				tail -c $$size /etc/ssl/certs/ca-certificates.crt | cmp - /usr/local/share/ca-certificates/extras.crt \
			' && echo "  $$img OK" || exit 1; \
		done; \
	fi
	@echo
	@echo "== observability stack pods ready =="
	kubectl --context $(KUBE_CTX) -n $(MONITORING_NS) wait --for=condition=Ready pod -l app.kubernetes.io/name=grafana --timeout=180s
	kubectl --context $(KUBE_CTX) -n $(MONITORING_NS) wait --for=condition=Ready pod -l app.kubernetes.io/name=tempo --timeout=180s
	kubectl --context $(KUBE_CTX) -n $(MONITORING_NS) wait --for=condition=Ready pod -l app.kubernetes.io/name=opentelemetry-collector --timeout=180s
	@echo
	@echo "== Grafana datasources healthy =="
	@kubectl --context $(KUBE_CTX) run gf-check --rm -i --restart=Never --image=curlimages/curl:8.10.1 --quiet -- \
		sh -c 'for ds in Prometheus Tempo Postgres; do \
		  code=$$(curl -sS -o /tmp/out -w "%{http_code}" -u admin:admin http://kube-prom-stack-grafana.$(MONITORING_NS).svc.cluster.local/api/datasources/name/$$ds); \
		  if [ "$$code" = "200" ]; then echo "  $$ds datasource OK"; else echo "  $$ds datasource FAIL ($$code)"; cat /tmp/out; exit 1; fi; \
		done'
	@echo
	@echo "== Dapr control plane pods ready =="
	kubectl --context $(KUBE_CTX) -n $(DAPR_NS) wait --for=condition=Ready pod --all --timeout=180s
	@echo
	@echo "== Data services ready =="
	kubectl --context $(KUBE_CTX) -n data wait --for=condition=Ready pod -l app=postgres --timeout=120s
	kubectl --context $(KUBE_CTX) -n data wait --for=condition=Ready pod -l app=redis    --timeout=60s
	kubectl --context $(KUBE_CTX) -n data wait --for=condition=Ready pod -l app=rabbitmq --timeout=180s
	@echo
	@echo "== Dapr Components registered =="
	@for c in state-postgres state-redis pubsub-rabbitmq secretstore-kubernetes; do \
	    kubectl --context $(KUBE_CTX) -n default get component $$c >/dev/null 2>&1 && echo "  $$c OK" || (echo "  $$c MISSING"; exit 1); \
	done
	@echo
	@echo "== ingest-svc pod ready with sidecar injected =="
	kubectl --context $(KUBE_CTX) wait --for=condition=Ready pod -l app=ingest-svc --timeout=90s
	@containers=$$(kubectl --context $(KUBE_CTX) get pod -l app=ingest-svc -o jsonpath='{.items[0].spec.containers[*].name}'); \
		echo "  containers: $$containers"; \
		echo "$$containers" | grep -qw ingest && echo "$$containers" | grep -qw daprd && echo "  sidecar injected OK" || (echo "  MISSING app or daprd container"; exit 1)
	@echo
	@echo "== ingest-svc responds through the Dapr sidecar =="
	@curl -sS -f -o - -w '\nHTTP %{http_code}\n' http://localhost:3500/v1.0/invoke/ingest-svc/method/health \
		|| (echo "  invocation failed — is Tilt up? is the ingest-svc dapr-http port-forward green?"; exit 1)
	@echo
	@echo "== Cost-center lookups seeded into state-redis =="
	@count=$$(kubectl --context $(KUBE_CTX) -n data exec deploy/redis -- redis-cli --no-raw KEYS 'ingest-svc||cost-center:*' | wc -l | tr -d ' '); \
	  echo "  cost-center keys in redis: $$count"; \
	  [ "$$count" -ge 7 ] || (echo "  expected at least 7 seeded cost centers"; exit 1)
	@echo
	@echo "== T7 end-to-end ingest flow — 20 synthetic line items =="
	@python3 data/generator/generate.py --day $$(date +%Y-%m-%d) --count 20 --seed 42 --url http://localhost:8080/ingest 2>&1 | tail -5
	@sleep 2
	@echo "  ingest-svc /stats:"
	@curl -sS http://localhost:8080/stats | sed 's/^/    /'
	@echo
	@echo "== Enriched line items in Postgres =="
	@kubectl --context $(KUBE_CTX) -n data exec deploy/postgres -- psql -U dapr -d state -tAc \
	  "SELECT count(*) FROM state WHERE key LIKE 'ingest-svc||line-item:%';" | (read n; echo "  line-item rows: $$n"; [ "$$n" -ge 15 ] || (echo "  expected \u226515 enriched rows (some unmapped are OK)"; exit 1))
	@echo
	@echo "== line-item.enriched messages arrived at RabbitMQ =="
	@kubectl --context $(KUBE_CTX) -n data exec deploy/rabbitmq -- rabbitmqctl list_queues name messages_ready messages_unacknowledged 2>/dev/null | grep -v Timeout | grep -v Listing | grep -E '(line-item|^name)'
	@echo
	@echo "== rollup-svc pod ready with sidecar injected =="
	kubectl --context $(KUBE_CTX) wait --for=condition=Ready pod -l app=rollup-svc --timeout=90s
	@echo
	@echo "== Rollups produced by rollup-svc in Postgres =="
	@sleep 3
	@echo "  rollup-svc /stats:"
	@curl -sS http://localhost:8081/stats | sed 's/^/    /'
	@kubectl --context $(KUBE_CTX) -n data exec deploy/postgres -- psql -U dapr -d state -tAc \
	  "SELECT count(*) FROM state WHERE key LIKE 'rollup-svc||rollup:%';" | (read n; echo "  rollup rows: $$n"; [ "$$n" -ge 1 ] || (echo "  expected \u22651 rollup"; exit 1))
	@echo "  top 3 rollups by cost:"
	@kubectl --context $(KUBE_CTX) -n data exec deploy/postgres -- psql -U dapr -d state -tAc \
	  "SELECT (v->>'team_id') || ' / ' || (v->>'service') || ' → ' || (v->>'count') || ' items, \$$' || (v->>'cost_usd') || ' USD' FROM (SELECT convert_from(value,'UTF8')::jsonb AS v FROM state WHERE key LIKE 'rollup-svc||rollup:%') s ORDER BY (v->>'cost_usd')::float DESC LIMIT 3" | sed 's/^/    /'
	@echo
	@echo "== Idempotency — re-seeding with same SEED does not double-count =="
	@before=$$(kubectl --context $(KUBE_CTX) -n data exec deploy/postgres -- psql -U dapr -d state -tAc \
	  "SELECT COALESCE(sum((convert_from(value,'UTF8')::jsonb->>'count')::int),0) FROM state WHERE key LIKE 'rollup-svc||rollup:%'"); \
	  echo "  total rolled-up items BEFORE re-seed: $$before"; \
	  dup_before=$$(curl -sS http://localhost:8081/stats | python3 -c 'import sys,json; print(json.load(sys.stdin)["duplicate"])'); \
	  python3 data/generator/generate.py --day $$(date +%Y-%m-%d) --count 20 --seed 42 --url http://localhost:8080/ingest 2>&1 | tail -2 | sed 's/^/    /'; \
	  sleep 4; \
	  after=$$(kubectl --context $(KUBE_CTX) -n data exec deploy/postgres -- psql -U dapr -d state -tAc \
	  "SELECT COALESCE(sum((convert_from(value,'UTF8')::jsonb->>'count')::int),0) FROM state WHERE key LIKE 'rollup-svc||rollup:%'"); \
	  echo "  total rolled-up items AFTER  re-seed: $$after"; \
	  dup_after=$$(curl -sS http://localhost:8081/stats | python3 -c 'import sys,json; print(json.load(sys.stdin)["duplicate"])'); \
	  echo "  duplicate deliveries detected by rollup-svc: $$dup_before → $$dup_after"; \
	  [ "$$before" = "$$after" ] || (echo "  IDEMPOTENCY BROKEN — totals changed from $$before to $$after"; exit 1); \
	  [ "$$dup_after" -gt "$$dup_before" ] || (echo "  expected duplicate counter to increase"; exit 1)
	@echo
	@echo "== A trace with service.name=ingest-svc has landed in Tempo =="
	@kubectl --context $(KUBE_CTX) run tempo-check --rm -i --restart=Never --image=curlimages/curl:8.10.1 --quiet -- \
		sh -c 'for i in 1 2 3 4 5 6 7 8 9 10; do \
		  resp=$$(curl -sS "http://tempo.$(MONITORING_NS).svc.cluster.local:3200/api/search?tags=service.name%3Dingest-svc&limit=5"); \
		  if echo "$$resp" | grep -q "traceID"; then echo "  found trace(s) with service.name=ingest-svc OK (after $${i}0s)"; exit 0; fi; \
		  sleep 3; \
		done; echo "  no traces found after 30s — check OTel Collector logs"; echo "$$resp"; exit 1'
	@echo
	@echo "== RBAC — pods run under dedicated SAs, secret-reader scoped correctly =="
	@sa=$$(kubectl --context $(KUBE_CTX) get pod -l app=ingest-svc -o jsonpath='{.items[0].spec.serviceAccountName}'); \
	  echo "  ingest-svc pod SA: $$sa"; \
	  [ "$$sa" = "ingest-svc-sa" ] || (echo "  expected ingest-svc-sa"; exit 1)
	@echo -n "  ingest-svc-sa can-i get secrets (should be NO — ingest doesn't need secrets): "
	@ans=$$(kubectl --context $(KUBE_CTX) auth can-i get secrets --as=system:serviceaccount:default:ingest-svc-sa -n default 2>/dev/null || true); \
	  echo "$$ans"; \
	  [ "$$ans" = "no" ] || (echo "  expected NO — no secret grant for ingest-svc"; exit 1)
	@echo
	@echo "== T9 anomaly detection — backfill history, spike today, batch detect =="
	@today=$$(date +%Y-%m-%d); \
	  echo "  backfilling 7 days of baseline (skipped days already present will be re-ingested and no-op via idempotency)..."; \
	  for i in 1 2 3 4 5 6 7; do \
	    day=$$(date -v-$${i}d +%Y-%m-%d 2>/dev/null || date -d "$$i days ago" +%Y-%m-%d); \
	    python3 data/generator/generate.py --day $$day --count 100 --seed $$((100+i)) --url http://localhost:8080/ingest 2>&1 | tail -1; \
	  done; \
	  echo "  seeding today with 4x spike on cc-payments-001/ec2..."; \
	  python3 data/generator/generate.py --day $$today --count 100 --seed 999 --spike cc-payments-001:ec2:4.0 --url http://localhost:8080/ingest 2>&1 | tail -1; \
	  sleep 5; \
	  echo "  triggering batch detection for $$today..."; \
	  resp=$$(curl -sS -X POST "http://localhost:8081/detect?day=$$today"); \
	  echo "    $$resp"; \
	  det=$$(echo "$$resp" | python3 -c 'import sys,json; print(json.load(sys.stdin)["detected"])'); \
	  dup=$$(echo "$$resp" | python3 -c 'import sys,json; print(json.load(sys.stdin)["duplicate"])'); \
	  echo "    → detected new: $$det, marked as duplicate: $$dup"; \
	  total=$$((det + dup)); \
	  [ "$$total" -ge 1 ] || (echo "  expected at least one anomaly (new or already-detected)"; exit 1); \
	  echo "  anomaly rows persisted in Postgres:"; \
	  kubectl --context $(KUBE_CTX) -n data exec deploy/postgres -- psql -U dapr -d state -tAc \
	    "SELECT count(*) FROM state WHERE key LIKE 'rollup-svc||anomaly:$$today:%'" | (read n; echo "    $$n"; [ "$$n" -ge 1 ] || (echo "    expected \u22651 anomaly persisted"; exit 1)); \
	  echo "  anomaly.detected queue in RabbitMQ (subscribers will exist in T11):"; \
	  kubectl --context $(KUBE_CTX) -n data exec deploy/rabbitmq -- sh -c 'rabbitmqctl list_exchanges name type 2>/dev/null | grep anomaly || echo "    (exchange auto-created on first publish)"'
	@echo
	@echo "== T10 Grafana FinOps dashboard =="
	@kubectl --context $(KUBE_CTX) -n $(MONITORING_NS) get configmap grafana-dashboards-finops -o jsonpath='{.metadata.labels.grafana_dashboard}' 2>/dev/null | (read v; if [ "$$v" = "1" ]; then echo "  ConfigMap grafana-dashboards-finops labelled OK"; else echo "  ConfigMap MISSING or unlabelled"; exit 1; fi)
	@kubectl --context $(KUBE_CTX) run gf-dash-check --rm -i --restart=Never --image=curlimages/curl:8.10.1 --quiet -- \
		sh -c 'code=$$(curl -sS -o /tmp/out -w "%{http_code}" -u admin:admin http://kube-prom-stack-grafana.$(MONITORING_NS).svc.cluster.local/api/dashboards/uid/finops-overview); \
		  if [ "$$code" = "200" ]; then \
		    title=$$(grep -o "\"title\":\"[^\"]*\"" /tmp/out | head -1 | cut -d\" -f4); \
		    echo "  Grafana dashboard loaded — uid=finops-overview, title=$$title  OK"; \
		  else echo "  dashboard NOT found ($$code)"; cat /tmp/out; exit 1; fi'
	@echo "  sample Postgres query via Grafana ds proxy:"
	@kubectl --context $(KUBE_CTX) run gf-ds-query --rm -i --restart=Never --image=curlimages/curl:8.10.1 --quiet -- \
		sh -c 'curl -sS -u admin:admin -X POST -H "Content-Type: application/json" \
		  -d "{\"queries\":[{\"refId\":\"A\",\"datasource\":{\"type\":\"postgres\",\"uid\":\"postgres\"},\"format\":\"table\",\"rawSql\":\"SELECT count(*) FROM state WHERE key LIKE '"'"'rollup-svc||rollup:%'"'"'\"}]}" \
		  http://kube-prom-stack-grafana.$(MONITORING_NS).svc.cluster.local/api/ds/query' \
	  | python3 -c 'import sys,json; d=json.load(sys.stdin); n=d["results"]["A"]["frames"][0]["data"]["values"][0][0]; print(f"    rollup rows visible via Grafana Postgres datasource: {n}")'
	@echo
	@echo "== T11 triage-svc + workflow =="
	kubectl --context $(KUBE_CTX) wait --for=condition=Ready pod -l app=triage-svc --timeout=90s
	@containers=$$(kubectl --context $(KUBE_CTX) get pod -l app=triage-svc -o jsonpath='{.items[0].spec.containers[*].name}'); \
	  echo "  triage-svc containers: $$containers"; \
	  echo "$$containers" | grep -qw triage && echo "$$containers" | grep -qw daprd && echo "  sidecar injected OK" || (echo "  MISSING triage or daprd"; exit 1)
	@kubectl --context $(KUBE_CTX) get subscription sub-anomaly-detected >/dev/null && echo "  Subscription sub-anomaly-detected OK" || (echo "  Subscription MISSING"; exit 1)
	@echo "  triage-svc /stats after anomaly batch:"
	@sleep 4
	@curl -sS http://localhost:8082/stats | sed 's/^/    /'
	@echo "  workflow instance for the most recent anomaly:"
	@aid=$$(kubectl --context $(KUBE_CTX) -n data exec deploy/postgres -- psql -U dapr -d state -tAc \
	  "SELECT convert_from(value,'UTF8')::jsonb->>'day' || ':' || (convert_from(value,'UTF8')::jsonb->>'team_id') || ':' || (convert_from(value,'UTF8')::jsonb->>'service') FROM state WHERE key LIKE 'rollup-svc||anomaly:%' ORDER BY key DESC LIMIT 1"); \
	  if [ -z "$$aid" ]; then echo "    no anomalies in postgres — did T9 run?"; exit 1; fi; \
	  wf_id="triage-anomaly-$$(echo $$aid | tr ':' '-')"; \
	  echo "    querying workflow id: $$wf_id"; \
	  resp=$$(curl -sS http://localhost:8082/workflows/$$wf_id); \
	  echo "$$resp" | python3 -m json.tool | sed 's/^/    /' | head -15; \
	  echo "$$resp" | grep -q '"name":"TriageWorkflow"' && echo "  workflow metadata retrievable OK" || (echo "  workflow NOT found — check triage-svc logs"; exit 1)

.PHONY: seed
seed: ## Post synthetic line items to ingest-svc (COUNT defaults to 100, DAY to today)
	python3 data/generator/generate.py --day $${DAY:-$$(date +%Y-%m-%d)} --count $${COUNT:-100} --seed $${SEED:-42} --url http://localhost:8080/ingest

.PHONY: backfill
backfill: ## Post 7 days of baseline synthetic history (yesterday .. 7 days ago)
	@for i in 1 2 3 4 5 6 7; do \
	  day=$$(date -v-$${i}d +%Y-%m-%d 2>/dev/null || date -d "$$i days ago" +%Y-%m-%d); \
	  echo "  backfilling day=$$day"; \
	  python3 data/generator/generate.py --day $$day --count 100 --seed $$((100+i)) --url http://localhost:8080/ingest 2>&1 | tail -1; \
	done

.PHONY: anomaly-demo
anomaly-demo: ## Seed today's data with a 4x spike on team-payments/ec2 and trigger detection
	@today=$$(date +%Y-%m-%d); \
	  echo "  seeding today ($$today) with 4x spike on cc-payments-001/ec2..."; \
	  python3 data/generator/generate.py --day $$today --count 100 --seed 999 --spike cc-payments-001:ec2:4.0 --url http://localhost:8080/ingest 2>&1 | tail -1; \
	  sleep 3; \
	  echo "  triggering batch detection..."; \
	  curl -sS -X POST "http://localhost:8081/detect?day=$$today" | python3 -m json.tool; \
	  echo "  rollup-svc /stats:"; \
	  curl -sS http://localhost:8081/stats | python3 -m json.tool | sed 's/^/    /'

## ---- Cluster lifecycle ----------------------------------------------------

.PHONY: cluster-ensure
cluster-ensure: ## Create the KinD cluster only if it does not already exist
	@if ! kind get clusters | grep -qx '$(CLUSTER_NAME)'; then \
		$(MAKE) cluster-create; \
	else \
		echo "KinD cluster '$(CLUSTER_NAME)' already exists"; \
	fi

.PHONY: cluster-create
cluster-create: prep ## Create the KinD cluster and install extra CAs into the node
	kind create cluster --name $(CLUSTER_NAME) --config $(KIND_CONFIG)
	$(MAKE) cluster-trust-cas

.PHONY: cluster-trust-cas
cluster-trust-cas: prep ## Install extra CAs into every KinD node's trust store and restart containerd
	@if [ ! -s $(CA_EXTRAS) ]; then \
		echo "  no extra CAs to install ($(CA_EXTRAS) is empty) — skipping"; \
		exit 0; \
	fi; \
	for node in $$(kind get nodes --name $(CLUSTER_NAME)); do \
		echo "  installing extra CAs into $$node"; \
		docker cp $(CA_EXTRAS) $$node:/usr/local/share/ca-certificates/extras.crt; \
		docker exec $$node update-ca-certificates >/dev/null; \
		docker exec $$node systemctl restart containerd; \
	done

.PHONY: cluster-delete
cluster-delete: ## Delete the KinD cluster
	-kind delete cluster --name $(CLUSTER_NAME)

## ---- Extra-CA bootstrap --------------------------------------------------
## Every build/cluster step reads `$(CA_EXTRAS)`. Its purpose: if your dev
## network sits behind a TLS-intercepting proxy (or otherwise needs custom CA
## trust), place a concatenated PEM bundle at `$(CA_EXTRAS)` before running
## `make up`. If the file is absent or empty, `make prep` creates it empty and
## every downstream consumer treats it as a no-op — the stack builds against
## the default public CAs.

.PHONY: prep
prep: $(CA_EXTRAS) ## Ensure $(CA_EXTRAS) exists (empty if you didn't provide one)

$(CA_EXTRAS):
	@touch $(CA_EXTRAS)
	@if [ -s $(CA_EXTRAS) ]; then \
		echo "  prep: $(CA_EXTRAS) present ($$(wc -c < $(CA_EXTRAS)) bytes)"; \
	else \
		echo "  prep: $(CA_EXTRAS) is empty — default CAs only"; \
	fi

## ---- Observability (Helm) -------------------------------------------------
## Installed once outside Tilt so app iteration doesn't churn on helm upgrade.

.PHONY: infra-repos
infra-repos: ## Add and update the Helm repos used by infra
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
	helm repo add grafana                https://grafana.github.io/helm-charts       >/dev/null
	helm repo add open-telemetry         https://open-telemetry.github.io/opentelemetry-helm-charts >/dev/null
	helm repo update >/dev/null

.PHONY: infra-install
infra-install: infra-repos ## Install kube-prometheus-stack + tempo + otel-collector + dashboards
	kubectl --context $(KUBE_CTX) create namespace $(MONITORING_NS) --dry-run=client -o yaml | kubectl --context $(KUBE_CTX) apply -f -
	helm --kube-context $(KUBE_CTX) upgrade --install kube-prom-stack prometheus-community/kube-prometheus-stack \
		--namespace $(MONITORING_NS) \
		--values deploy/infra/values/kube-prom-stack.yaml \
		--wait --timeout 5m
	helm --kube-context $(KUBE_CTX) upgrade --install tempo grafana/tempo \
		--namespace $(MONITORING_NS) \
		--values deploy/infra/values/tempo.yaml \
		--wait --timeout 3m
	helm --kube-context $(KUBE_CTX) upgrade --install otel-collector open-telemetry/opentelemetry-collector \
		--namespace $(MONITORING_NS) \
		--values deploy/infra/values/otel-collector.yaml \
		--wait --timeout 3m
	$(MAKE) dashboards-install

.PHONY: dashboards-install
dashboards-install: ## Provision the FinOps Grafana dashboard as a labelled ConfigMap
	@# Wraps every JSON under deploy/infra/grafana-dashboards/ into a single
	@# ConfigMap in the monitoring namespace, labelled so kube-prom-stack's
	@# Grafana sidecar auto-loads them.
	kubectl --context $(KUBE_CTX) create configmap grafana-dashboards-finops \
		--namespace $(MONITORING_NS) \
		--from-file=deploy/infra/grafana-dashboards/ \
		--dry-run=client -o yaml \
	| kubectl --context $(KUBE_CTX) label -f - --local --overwrite \
		grafana_dashboard=1 \
		-o yaml \
	| kubectl --context $(KUBE_CTX) apply -f -

.PHONY: infra-uninstall
infra-uninstall: ## Uninstall the observability stack (keeps the cluster)
	-helm --kube-context $(KUBE_CTX) uninstall otel-collector -n $(MONITORING_NS)
	-helm --kube-context $(KUBE_CTX) uninstall tempo          -n $(MONITORING_NS)
	-helm --kube-context $(KUBE_CTX) uninstall kube-prom-stack -n $(MONITORING_NS)
	-kubectl --context $(KUBE_CTX) delete namespace $(MONITORING_NS)

## ---- Dapr (Helm) ----------------------------------------------------------

.PHONY: dapr-install
dapr-install: infra-repos ## Install the Dapr control plane into dapr-system
	helm --kube-context $(KUBE_CTX) upgrade --install dapr dapr/dapr \
		--namespace $(DAPR_NS) --create-namespace \
		--values deploy/infra/values/dapr.yaml \
		--wait --timeout 5m

.PHONY: dapr-uninstall
dapr-uninstall: ## Uninstall Dapr (keeps the cluster)
	-helm --kube-context $(KUBE_CTX) uninstall dapr -n $(DAPR_NS)
	-kubectl --context $(KUBE_CTX) delete namespace $(DAPR_NS)

## ---- Meta -----------------------------------------------------------------

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
