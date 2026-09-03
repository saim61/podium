# Podium

**A real-time leaderboard system.** Five server-authoritative games feed per-game and cross-game
leaderboards built on Redis sorted sets, with ranks that update in the browser as scores land.

Built in Go against the [roadmap.sh real-time leaderboard brief](https://roadmap.sh/projects/realtime-leaderboard-system).

> **Status:** in progress. Phases 0–1 of 10 complete — configuration, structured logging, the HTTP
> error envelope, middleware, health probes, a container stack, and the persistence layer with
> migrations and container-backed integration tests. **80% coverage.** See [Roadmap](#roadmap).

---

## Why this exists

The brief asks for score submission, leaderboards, rankings and period reports. Read literally,
that is a `POST /scores` handler that trusts whatever number the client sends, and it proves
nothing — the interesting problems are all in what that reading skips.

Three decisions define this implementation.

**The server owns the games.** There is no endpoint that accepts a score. Each of the five games is
a state machine living on the server: it holds the secret, validates every move, enforces the
deadline on its own clock, and computes the final score itself. A client cannot forge a score
because it never sends one. This is the difference between a leaderboard and a suggestion box.

**Postgres is the source of truth; Redis is a derived read model.** Scores are durably recorded in
an append-only table, and the sorted sets are a projection of that table. Redis can be flushed at
any moment and the system rebuilds itself. Treating Redis as a database — the obvious way to build
this — means one `FLUSHALL` or one evicted key silently destroys the product.

**Realtime has to survive horizontal scaling.** An in-process hub broadcasting to local WebSocket
connections works perfectly on one instance and breaks the moment there are two: a score submitted
to instance A never reaches a client connected to instance B. Updates therefore travel over Redis
Pub/Sub, and the design is verified by actually running two instances rather than by assertion.

---

## Quick start

**Requirements:** Go 1.26+, Docker.

```bash
git clone git@github.com:saim61/podium.git && cd podium
docker compose up -d --build
```

That gives you Postgres, Redis, a one-shot migration job and the API. Check it:

```bash
curl -s localhost:8080/healthz   # {"status":"ok"} - the process is up
curl -s localhost:8080/readyz    # per-dependency status, 503 if any are down
```

For day-to-day development, run only the infrastructure in Docker and keep the API on the host,
where a debugger can attach:

```bash
docker compose up -d db redis
go run ./cmd/migrate
go run ./cmd/api
```

```bash
go test ./...              # unit and integration tests
go test -cover ./...       # with coverage
gofmt -l .                 # should print nothing
go vet ./...
```

CI additionally runs the suite under `-race`, which is where the concurrent parts of this system —
the leaderboard projection and the realtime hub — are actually held to account. It is not in the
list above because `-race` requires cgo and therefore a C toolchain, which a stock Windows Go
install does not have. Don't take a locally green suite as evidence of race-freedom.

After changing a query or a migration, regenerate the typed query layer:

```bash
docker run --rm -v "$PWD:/src" -w /src sqlc/sqlc:1.31.1 generate
```

sqlc runs from its pinned image rather than as a Go tool dependency. Adding it to `go.mod` pulled
protobuf, grpc and a wasm runtime into the module graph — 217 modules, up from about 10 — which
slowed every Docker build for a tool the image never runs. The generated code is committed, so
neither CI nor the image build needs sqlc at all.

If port 5432 is already taken by a local Postgres, change the mapping in `compose.yaml` to
`"5433:5432"` and set `PODIUM_DATABASE_URL` to match.

---

## Configuration

Every setting is an environment variable prefixed `PODIUM_`, and every one has a working local
default — see [`.env.example`](.env.example) for the full list with its defaults.

| Variable | Default | Purpose |
|---|---|---|
| `PODIUM_ENV` | `dev` | `dev` or `prod`. Controls how much detail `/readyz` reveals. |
| `PODIUM_HTTP_ADDR` | `:8080` | Listen address. |
| `PODIUM_HTTP_SHUTDOWN_TIMEOUT` | `15s` | How long in-flight requests get to finish on SIGTERM. |
| `PODIUM_DATABASE_URL` | local | Postgres connection string. |
| `PODIUM_REDIS_URL` | local | Redis connection string. |
| `PODIUM_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `PODIUM_LOG_FORMAT` | `json` | `json` to ship, `text` to read in a terminal. |

Configuration is validated once at boot, and **every** problem is reported together:

```
api: invalid configuration: PODIUM_HTTP_READ_TIMEOUT: "soon" is not a duration (try 30s, 5m, 1h)
PODIUM_LOG_LEVEL: "chatty" is not one of debug, info, warn, error
```

Failing on the first error instead would mean one restart per mistake to discover them all, which
is a miserable way to bring up a new deployment.

---

## Health probes

```
GET /healthz   liveness  — always 200 while the process runs. Touches nothing.
GET /readyz    readiness — probes each dependency in parallel. 503 if any fail.
```

These are separate on purpose, because orchestrators treat them very differently: a failing
liveness probe **restarts** the container, while a failing readiness probe merely pulls it out of
the load balancer. If liveness checked Postgres, a thirty-second database blip would restart every
replica simultaneously — turning a recoverable outage into a much worse one, right when the
database can least afford a thundering herd of reconnects.

`/readyz` reports dependency error text in `dev` and withholds it in `prod`:

```jsonc
// dev
{"status":"unavailable","checks":{"postgres":{"status":"error",
  "error":"dial tcp 10.0.0.5:5432: connection refused"}}}

// prod — same status, no internal topology
{"status":"unavailable","checks":{"postgres":{"status":"error"}}}
```

In development that error string is the whole value of the probe. In production, an unauthenticated
endpoint that prints internal hostnames and ports is free reconnaissance.

---

## Persistence

Postgres is the source of truth. Redis, once the leaderboards arrive in phase 4, holds only a
projection of it.

### Migrations run as a job, never on boot

```bash
go run ./cmd/migrate      # or the `migrate` service in compose
```

The API never migrates itself. Two API replicas starting together would run `goose up`
concurrently and race on the version table, and the migration runner is not where you want to
discover that. In compose the `migrate` service is a one-shot job and the API declares
`depends_on: {migrate: {condition: service_completed_successfully}}`, so nothing serves traffic
against a schema that is missing the tables it is about to query. Migrations are embedded in the
binary with `embed.FS`, so the running image and its schema can never drift apart.

Applying twice is a no-op — the second run reports `applied: 0` — which is what makes it safe to
put in front of every deployment.

### User ids are `bigint`, not UUIDs

This is the one schema decision worth arguing about, and it is driven entirely by Redis.

A leaderboard entry is a **sorted set member**, and a member is stored as a string. A UUID is 36
characters; a `bigint` id is around 7. With five games across four periods plus the cross-game
sets — 24 sorted sets — that difference is the difference between roughly 190 MB and 860 MB of
Redis at a million users. Sorted set memory *is* the scaling constraint in this system, so the
identifier that lands in every one of them should be small.

The usual objection to sequential ids is that they leak and enumerate. That does not apply here
because **the numeric id is never public**. Leaderboards are about people, so the API speaks
usernames; a caller asking about themselves is identified by their token, not by an id in a URL.
Entries are keyed internally by id and hydrated to the current username at read time, which also
means a rename is reflected across every historical leaderboard for free.

Where an identifier *does* appear in a URL — game sessions, in phase 3 — it will be a UUID, since
that one is public and sequential session ids would let a user probe for other people's sessions.

### Uniqueness ignores case

```sql
CREATE UNIQUE INDEX users_username_key ON users (lower(username));
CREATE UNIQUE INDEX users_email_key    ON users (lower(email));
```

A plain `UNIQUE` constraint would happily let `saeem` and `SAEEM` both register, which is an
account-confusion and impersonation problem on a leaderboard where the username is the identity
everyone sees. Functional indexes on `lower()` make the database enforce it, so the rule holds no
matter which code path inserts — and the same index serves the case-insensitive login lookup, so
it costs nothing.

### Pools connect lazily

`postgres.Open` and `redis.Open` build a pool and return; neither dials. A process that refuses to
start because a dependency is briefly unreachable crash-loops, and a crash-looping deployment adds
a reconnect storm to an outage while making rollout status useless. Podium starts, reports itself
**unready**, gets pulled from the load balancer, and recovers on its own when the dependency does.
Reachability is `/readyz`'s job, and it says so per dependency.

### Queries are generated, not hand-scanned

Typed query functions come from [sqlc](https://sqlc.dev), which reads the migrations as the schema
and the files in `internal/store/queries/` as the queries. A `SELECT` that names a column that
does not exist fails at generation time rather than in production, and there is no hand-written
`rows.Scan` to fall out of order with a changed `SELECT` list.

The generated structs carry no JSON tags on purpose. They are row types, not API types — letting
them serialise directly is how `password_hash` ends up in a response body.

## Errors

Every failure uses one envelope, so a client parses one shape:

```json
{
  "error": {
    "code": "bad_request",
    "message": "username is taken",
    "fields": {"username": "already registered"},
    "request_id": "68740fe9f38a5f97"
  }
}
```

`request_id` is the same id echoed in the `X-Request-Id` response header and attached to every log
line for that request, so a user-reported failure can be traced to its logs by copying one string.

Unexpected errors are never rendered verbatim. An internal failure returns a fixed
`"an unexpected error occurred"` and the real cause goes to the logs — error strings routinely
contain connection strings, file paths and query fragments, and a 500 is not the place to publish
them. There is a test asserting a Postgres password in an error cannot reach a response body.

JSON decoding rejects unknown fields. A client that sends `{"user_name": "x"}` when the API expects
`username` gets an error rather than a silently-ignored zero value.

---

## Testing

Integration tests run against **real Postgres and Redis** in throwaway containers
([testcontainers](https://golang.testcontainers.org)), not mocks.

This is a deliberate cost. The container starts in about three seconds and the whole suite runs in
seven. What it buys is that the tests exercise the things this system actually depends on: that a
functional unique index really does reject `SAEEM` after `saeem`, that a missing row really does
come back as `pgx.ErrNoRows`, and — from phase 4 — that a Lua script really is atomic against
concurrent writers. A mocked store can only assert that Podium called the functions Podium calls,
which is a restatement of the implementation, not a test of it.

One container serves the whole test binary; isolation comes from truncating every table with
`RESTART IDENTITY` before each test, which costs milliseconds instead of seconds. `go test -short`
skips the container-backed tests when Docker isn't available.

Coverage is measured with `-coverpkg` across all packages. Without it, code reached only through
the integration suite reports 0% — because the tests live in a different package than the code —
and the headline number becomes fiction.

## Roadmap

| # | Phase | Status |
|---|---|---|
| 0 | Skeleton — config, logging, error envelope, middleware, probes, containers, CI | done |
| 1 | Persistence — migrations, pgx pool, generated queries, container-backed tests | done |
| 2 | Auth — register/login/refresh, Argon2id, JWT, refresh rotation with reuse detection | next |
| 3 | Games and sessions — five engines, server-side deadlines, seeded and replayable | |
| 4 | Leaderboards — sorted sets, atomic submission in Lua, top-N, rank, neighbours | |
| 5 | Consistency — projection cursor, self-healing drift, rebuild from Postgres | |
| 6 | Realtime — WebSockets, Pub/Sub fan-out, coalescing, slow-client eviction | |
| 7 | Reports and rate limiting — hot and cold period reports, token buckets | |
| 8 | Demo UI — play all five games, watch ranks reorder live | |
| 9 | Production — metrics, OpenAPI, load test results, deployment on two instances | |

## Layout

```
cmd/api/                     the HTTP server
cmd/migrate/                 one-shot schema migration job
internal/config/             environment configuration, validated at boot
internal/httpapi/            router, middleware, error envelope, health probes
internal/platform/           logging, request context, Postgres and Redis clients
internal/store/migrations/   embedded .sql migrations and the runner
internal/store/queries/      hand-written SQL, input to sqlc
internal/store/db/           sqlc-generated query functions (committed)
internal/testsupport/        container fixtures for integration tests
tests/integration/           tests that need real Postgres and Redis
```

## Conventions

Code carries no narration. A comment appears only where it explains something the code cannot —
a non-obvious invariant, or why an approach was rejected. Everything else that needs explaining
lives in this README, where it can be read as an argument instead of reassembled from fragments.
