# Podium

**A real-time leaderboard system.** Five server-authoritative games feed per-game and cross-game
leaderboards built on Redis sorted sets, with ranks that update in the browser as scores land.

Built in Go against the [roadmap.sh real-time leaderboard brief](https://roadmap.sh/projects/realtime-leaderboard-system).

> **Status:** in progress. Phase 0 of 10 complete — configuration, structured logging, the HTTP
> error envelope, middleware, health probes and a container stack. See [Roadmap](#roadmap).

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

That gives you Postgres, Redis and the API. Check it:

```bash
curl -s localhost:8080/healthz   # {"status":"ok"} - the process is up
curl -s localhost:8080/readyz    # per-dependency status, 503 if any are down
```

For day-to-day development, run only the infrastructure in Docker and keep the API on the host,
where a debugger can attach:

```bash
docker compose up -d db redis
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

## Roadmap

| # | Phase | Status |
|---|---|---|
| 0 | Skeleton — config, logging, error envelope, middleware, probes, containers, CI | done |
| 1 | Persistence — migrations, pgx pool, generated queries, container-backed tests | next |
| 2 | Auth — register/login/refresh, Argon2id, JWT, refresh rotation with reuse detection | |
| 3 | Games and sessions — five engines, server-side deadlines, seeded and replayable | |
| 4 | Leaderboards — sorted sets, atomic submission in Lua, top-N, rank, neighbours | |
| 5 | Consistency — projection cursor, self-healing drift, rebuild from Postgres | |
| 6 | Realtime — WebSockets, Pub/Sub fan-out, coalescing, slow-client eviction | |
| 7 | Reports and rate limiting — hot and cold period reports, token buckets | |
| 8 | Demo UI — play all five games, watch ranks reorder live | |
| 9 | Production — metrics, OpenAPI, load test results, deployment on two instances | |

## Layout

```
cmd/api/              the HTTP server
internal/config/      environment configuration, validated at boot
internal/httpapi/     router, middleware, error envelope, health probes
internal/platform/    logging and request-scoped context
```

## Conventions

Code carries no narration. A comment appears only where it explains something the code cannot —
a non-obvious invariant, or why an approach was rejected. Everything else that needs explaining
lives in this README, where it can be read as an argument instead of reassembled from fragments.
