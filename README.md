# order-engine

A production-hardened pet project: an order-processing microservice that
serves **Connect, gRPC, and gRPC-Web on a single h2c `net/http` server**,
stores state in **PostgreSQL** (`database/sql` + `pgx/v5/stdlib`), emits
**OpenTelemetry** traces, and ships with a complete deployment story:
distroless Docker image, Helm chart, **Gateway API** routing, cert-manager
TLS, and Argo CD GitOps.

```
Client ──TLS──▶ Gateway (edge TLS termination)
                  │ h2c (GRPCRoute / HTTPRoute)
                  ▼
        Service :50051  ──▶ Pod (Connect + gRPC + gRPC-Web + health)
                  │
        Headless Service ◀── native gRPC clients (custom order_random LB)
                  │
                  ▼
              PostgreSQL
```

## Trust model and scope — read this first

- **The bearer-token auth here is NOT production authentication.** The
  server validates `Authorization: Bearer <token>` through an injected
  `TokenValidator` interface; the bundled `StaticTokenValidator`
  (default token `token-123` → account `acct-demo`) exists for development
  and tests only. Production deployments must inject a real validator
  (OIDC/JWT/mTLS-derived identity, ...) behind the same interface.
- **Edge TLS, internal h2c.** TLS terminates at the Gateway. Traffic from
  the Gateway to Pods, and from in-cluster clients to the headless Service,
  is cleartext HTTP/2 (h2c) inside the cluster network. This is edge TLS,
  not end-to-end TLS/mTLS; if you need in-mesh encryption, add a mesh or
  pod-level TLS on top.
- **The custom load balancer applies to native grpc-go clients only.** The
  `order_random` policy (`pkg/customlb`) plugs into grpc-go's balancer
  registry. Connect clients use a plain `http.Client` and perform no
  client-side load balancing or automatic retries.
- **The headless Service is why the picker sees Pod IPs.** grpc-go resolves
  `dns:///order-engine-headless...` to every ready Pod IP and the picker
  chooses among them per call. Dialing the normal ClusterIP Service would
  yield one virtual IP and there would be nothing to balance.
- **Retrying `CreateOrder` is safe only because of end-to-end idempotency.**
  Every request carries an `idempotency_key`; the database enforces a named
  unique constraint on `(account_id, idempotency_key)`, and a retry —
  automatic or manual, on any protocol — returns the original order without
  a second balance deduction. Never enable retry policies for RPCs that do
  not have this property.
- **Money is integral.** Amounts are `int64` minor units plus an ISO 4217
  currency code; floating point is never used.

## Repository layout

| Path | Purpose |
|---|---|
| `proto/order/v1` | Protobuf schema (source of truth) |
| `gen/` | Buf-generated code — never edited by hand |
| `pkg/xsql` | `SQLExecutor`, context transaction propagation, generic `QuerySingle[T]` |
| `pkg/dataservicex` | Generic repository base with identifier-safe SQL |
| `pkg/customlb` | `order_random` grpc-go balancer |
| `pkg/slogotel` | slog handler adding `trace_id`/`span_id`/`trace_sampled` (log/trace correlation only, not an OTel log exporter) |
| `internal/domain`, `internal/usecase` | Business model + `PlaceOrder` use case (owns its ports) |
| `internal/repository/postgres` | PostgreSQL adapters + `SQLTransactor` |
| `internal/transport/connect` | Connect handler, auth interceptor, error mapper, health |
| `cmd/api`, `cmd/client` | Server and native-gRPC demo client |
| `migrations/`, `scripts/seed.sql` | Schema + deterministic dev seed |
| `deployments/` | Helm chart, platform (Gateway, cert-manager), Argo CD |

## Quickstart (local, Docker Compose)

```bash
docker compose up --build
# in another terminal — Connect protocol via curl:
curl -X POST http://127.0.0.1:50051/order.v1.OrderService/CreateOrder \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer token-123" \
  -d '{"idempotencyKey":"demo-1","amountMinor":"1234","currency":"USD"}'
# native gRPC with the custom LB + retry config:
go run ./cmd/client -target 127.0.0.1:50051 -idempotency-key demo-2
```

PostgreSQL initializes itself once from `migrations/000001_init.up.sql` and
`scripts/seed.sql` (mounted into `docker-entrypoint-initdb.d`), seeding
`acct-demo` with 1,000,000 minor units of USD.

## Migrations

There is no migration framework and **serving Pods never migrate at
startup** — five replicas racing on `CREATE TABLE` is exactly the failure
mode this design avoids. There are two controlled paths:

- **Deployment (the default):** the Helm chart runs `order-engine migrate`
  as a `pre-install,pre-upgrade` hook Job (`migrations.hookEnabled`), so
  exactly one runner executes per rollout, before any Pod restarts. The
  migrations are embedded in the binary (`migrations/` stays the single
  source of truth), and the runner takes a PostgreSQL **advisory lock**,
  tracks applied files in `schema_migrations`, and commits each migration
  together with its version record in one transaction — concurrent or
  repeated invocations are safe no-ops.
- **Local development:** plain `psql` in filename order, each file wrapped
  in one transaction by `psql -1`:

```bash
make migrate-up            # apply all *.up.sql
make migrate-down          # apply all *.down.sql in reverse
make db-seed               # deterministic dev seed (re-runnable)
make db-reset              # down + up + seed
```

Migration files carry no `BEGIN/COMMIT` (the runner or `psql -1` owns the
transaction) and are intentionally **not** idempotent (`CREATE TABLE IF NOT
EXISTS` masks drift); the tracked runner is what makes re-runs no-ops. The
constraint name `uq_orders_account_idempotency_key` is load-bearing: the
repository matches it when translating unique violations into idempotency
conflicts, and the integration suite fails if migration and code drift.

## Building and testing

```bash
make tools        # pinned buf into ./bin (helm: see target output)
make build        # buf lint + buf generate + go build
make test         # go test -race ./... incl. real-PostgreSQL integration tests
```

Integration tests need an admin DSN in `ORDER_ENGINE_TEST_DATABASE_URL`
(e.g. `postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable`); each
test creates, migrates (via `psql`, same mechanism as `make migrate-up`) and
drops its own throwaway database. Without the variable the tests skip —
except under `ORDER_ENGINE_TEST_REQUIRE_DB=1` (set by `make test` and CI),
which turns skips into failures so the suite can never silently thin out.

The suite covers, against real PostgreSQL and a real server: all three wire
protocols, cross-protocol idempotent retry, `ErrorInfo` decoding through
Connect and grpc-go clients, health transitions, two barrier-synchronized
concurrent requests on one key (one order, one deduction, same order ID),
and fault drills that sever connections mid-RPC to prove lost
requests/responses are retried without double-charging.

### A note on gRPC retry vs Connect servers

grpc-go applies a `retryPolicy` only to **trailers-only** failures
(gRFC A6). A `net/http`-based Connect server cannot emit trailers-only gRPC
errors, so a server-*returned* `UNAVAILABLE` status is not auto-retried —
the integration suite pins this behavior. Transport-level failures
(connection lost before response headers) *are* retried per policy, which is
the case that matters for lost responses; application callers should still
retry `UNAVAILABLE` manually with the same idempotency key.

## Server configuration

Configuration is layered with [koanf](https://github.com/knadh/koanf)
(the one dependency admitted beyond the original allowlist, for config
only). Precedence, lowest to highest:

1. built-in defaults;
2. an optional YAML file named by `ORDER_ENGINE_CONFIG_FILE`
   (see `config.example.yaml` — a configured-but-unreadable file fails
   loudly);
3. environment variables.

Every YAML key equals its environment name minus the `ORDER_ENGINE_`
prefix, lowercased. Invalid values are reported **all at once**, not one
per restart. Keep secrets (DSN, tokens) in the environment/Secret, not in
the file.

| Variable | Default | Notes |
|---|---|---|
| `ORDER_ENGINE_LISTEN_ADDR` | `0.0.0.0:50051` | h2c: HTTP/1.1 + HTTP/2 prior knowledge |
| `ORDER_ENGINE_DATABASE_URL` | — (required) | never logged |
| `ORDER_ENGINE_AUTH_TOKEN` | `token-123` | dev/test static validator only |
| `ORDER_ENGINE_AUTH_ACCOUNT_ID` | `acct-demo` | account the dev token maps to |
| `ORDER_ENGINE_ALLOW_DEV_AUTH` | `false` | fail-closed guardrail: startup refuses the built-in `token-123` unless this is `true` (compose sets it for local dev) |
| `ORDER_ENGINE_TRACING_ENABLED` | `true` | OTLP/gRPC exporter, endpoint via standard `OTEL_EXPORTER_OTLP_*` |
| `ORDER_ENGINE_SHUTDOWN_TIMEOUT` | `20s` | drain budget on SIGTERM |
| `ORDER_ENGINE_DB_MAX_OPEN_CONNS` etc. | `10/10/30m/5m` | pool sizing |
| `ORDER_ENGINE_CONFIG_FILE` | — | optional YAML layer between defaults and env |

Health: gRPC health service names `liveness` (process up, no DB dependency)
and `readiness` (starts NOT_SERVING; SERVING only after `db.PingContext`
succeeds; NOT_SERVING again the moment SIGTERM arrives). Shutdown order:
readiness off → drain server → flush OTel → close DB. The chart adds a
shell-free native `preStop` sleep (Kubernetes >= 1.30 SleepAction) so the
pod keeps accepting traffic while EndpointSlice removal propagates; the
distroless image needs no shell for any of this. The chart also pins
`GOMEMLIMIT` just below the memory limit so heap pressure triggers GC
instead of the OOM killer, and sets no CPU limit — Go 1.25+ sizes
GOMAXPROCS from the cgroup quota on its own.

## Deployment

### Prerequisites (install before the app chart)

1. **Gateway API CRDs** (Standard Channel, `gateway.networking.k8s.io/v1`)
   and a **Gateway controller with `GRPCRoute` conformance** (e.g. Envoy
   Gateway, Cilium, Istio). The chart creates Routes only; it references an
   existing Gateway via `parentRefs` and creates no GatewayClass.
2. **cert-manager** with Gateway API support enabled
   (`config.gatewayAPI.enabled=true` / `enableGatewayAPI: true` in its
   controller config — and the Gateway API CRDs must exist *before*
   cert-manager starts).
3. Public DNS for your hostname pointing at the Gateway address before ACME
   HTTP-01 challenges can pass. Wildcard certificates require DNS-01
   instead.
4. Kubernetes **>= 1.34** (chart `kubeVersion` enforces it; native gRPC
   probes and every API used are stable there).

Platform manifests (`deployments/platform/`) provide the shared Gateway
(listener :80 for HTTP-01 + :443 terminating TLS) and cert-manager
issuers/Certificate; the app chart (`deployments/helm/order-engine`) renders
namespace-scoped resources only.

### Routing modes

`routing.mode` selects exactly one Route kind for `routing.hostname`:

- `grpc` — a `GRPCRoute` for native gRPC through the Gateway.
- `http` — an `HTTPRoute` for the Connect protocol and gRPC-Web (or mixed
  HTTP+gRPC on one hostname).

The two modes never render simultaneously, so one hostname is never claimed
by overlapping Routes.

```bash
helm template order-engine deployments/helm/order-engine \
  --kube-version 1.34.0 \
  --set routing.mode=grpc --set routing.hostname=grpc.example.test
```

### GitOps

`deployments/argocd/` contains a dedicated `AppProject` (repo/destination/
kind allowlists), a conservatively-pruned platform Application, and the app
Application (automated sync, prune, self-heal, retry with backoff,
`CreateNamespace`/`PruneLast`/`FailOnSharedResource`). Sync order: Gateway
CRDs/controller → cert-manager → issuers/Gateway/Certificate → app. Argo CD
deploys only immutable image references committed to Git — it never builds
images. Production values pin the image by digest.

## Observability

- `connectrpc.com/otelconnect` server interceptor sits **outside** auth, so
  failed authentication still produces spans. Propagators: W3C TraceContext
  + Baggage. Exporter: OTLP over gRPC through a `BatchSpanProcessor`,
  resource `service.name=order-engine`.
- Logs are JSON via `pkg/slogotel`: records logged with `InfoContext`/
  `ErrorContext` gain `trace_id`, `span_id`, `trace_sampled` whenever a
  valid span context is present.

## Error contract

Domain failures map via `errors.Is`/`errors.As` to Connect codes with a
`google.rpc.ErrorInfo` detail (`domain=order-engine.acme.example`, reasons
`INVALID_ARGUMENT`, `NOT_FOUND`, `INSUFFICIENT_BALANCE`,
`IDEMPOTENCY_KEY_CONFLICT`); both Connect and grpc-go clients can decode it.
Unclassified errors surface as `internal error` with no detail — SQL, DSNs,
and driver internals never reach clients.
