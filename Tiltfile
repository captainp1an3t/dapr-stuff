# Tiltfile — dapr-stuff
#
# Task T1 wires up the infra spine: base images with any extra CAs baked in,
# and a trivial hello service to prove the loop end-to-end.
#
# Later tasks will extend this file with Dapr, observability, and the real services.

# ---- Safety: only run against our KinD cluster ----
allow_k8s_contexts('kind-dapr-stuff')

# ---- Shared base images (built first; per-service Dockerfiles FROM them) ----
docker_build(
    'dapr-stuff/base-builder',
    context='.',
    dockerfile='base.Dockerfile',
    target='builder-base',
    only=['base.Dockerfile', '.ca-extras.pem'],
)

docker_build(
    'dapr-stuff/base-runtime',
    context='.',
    dockerfile='base.Dockerfile',
    target='runtime-base',
    only=['base.Dockerfile', '.ca-extras.pem'],
)

# ---- Services ----

# ---- Kubernetes manifests ----
# Register Dapr CRD kinds explicitly. Tilt's API-discovery cache is populated
# at startup; if Dapr CRDs get installed after Tilt starts (the normal `make up`
# order), Tilt won't know about them and will fail with "no matches for kind".
# Declaring here bypasses discovery.
k8s_kind('Configuration', api_version='dapr.io/v1alpha1')
k8s_kind('Component',     api_version='dapr.io/v1alpha1')
k8s_kind('Subscription',  api_version='dapr.io/v2alpha1')

# Data services (Postgres, Redis, RabbitMQ) — Tilt-managed so their pods show
# in the UI.
k8s_yaml([
    'deploy/infra/data/postgres.yaml',
    'deploy/infra/data/redis.yaml',
    'deploy/infra/data/rabbitmq.yaml',
])
k8s_resource(
    'postgres',
    port_forwards='5432:5432',
    objects=['data:namespace'],  # attach the data Namespace to postgres so it doesn't float
    labels=['data'],
)
k8s_resource('redis', port_forwards='6379:6379', labels=['data'])
k8s_resource(
    'rabbitmq',
    port_forwards=[
        port_forward(5672, 5672, name='amqp'),
        port_forward(15672, 15672, name='mgmt'),
    ],
    links=[link('http://localhost:15672', 'RabbitMQ management (dapr / daprdemo)')],
    labels=['data'],
)

# Dapr Configuration + Components + Subscriptions.
# Tilt auto-groups these under the workloads that reference them (via
# annotations or scopes) — do NOT try to group manually via objects= or Tilt
# will error with "no object identified".
# Dapr Configuration + Components + Subscriptions.
# Tilt auto-groups these under the workloads that reference them (via
# annotations or scopes) — do NOT try to group manually via objects= or Tilt
# will error with "no object identified".
k8s_yaml([
    'deploy/dapr/config-tracing.yaml',
    'deploy/dapr/state-postgres.yaml',
    'deploy/dapr/state-redis.yaml',
    'deploy/dapr/pubsub-rabbitmq.yaml',
    'deploy/dapr/secretstore-kubernetes.yaml',
    'deploy/dapr/subscription-line-item-enriched.yaml',
])

# ---- Services ----
docker_build(
    'dapr-stuff/ingest-svc',
    context='services/',
    dockerfile='services/ingest-svc/Dockerfile',
    only=['ingest-svc/', 'shared/'],
    ignore=['**/*.md', '**/*_test.go'],
)

docker_build(
    'dapr-stuff/rollup-svc',
    context='services/',
    dockerfile='services/rollup-svc/Dockerfile',
    only=['rollup-svc/', 'shared/'],
    ignore=['**/*.md', '**/*_test.go'],
)

k8s_yaml([
    'deploy/apps/ingest-svc.yaml',
    'deploy/apps/rollup-svc.yaml',
    'deploy/apps/secret-demo.yaml',
])
k8s_resource(
    'ingest-svc',
    port_forwards=[
        port_forward(8080, 8080, name='app'),
        port_forward(3500, 3500, name='dapr-http'),
    ],
    labels=['apps'],
)
k8s_resource(
    'rollup-svc',
    port_forwards=[
        port_forward(8081, 8080, name='app'),
    ],
    labels=['apps'],
)

# ---- Observability port-forwards ----
# The observability stack is installed via `make infra-install` (Helm) outside
# Tilt so app iteration doesn't churn Helm on every save. Tilt owns the
# port-forwards so the UI has one place to click into each dashboard.
local_resource(
    'grafana',
    serve_cmd='kubectl --context kind-dapr-stuff port-forward -n monitoring svc/kube-prom-stack-grafana 3000:80',
    links=[link('http://localhost:3000', 'Grafana (admin / admin)')],
    labels=['observability'],
    allow_parallel=True,
)

local_resource(
    'prometheus',
    serve_cmd='kubectl --context kind-dapr-stuff port-forward -n monitoring svc/kube-prom-stack-kube-prome-prometheus 9090:9090',
    links=[link('http://localhost:9090', 'Prometheus')],
    labels=['observability'],
    allow_parallel=True,
)
