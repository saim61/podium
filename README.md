# Podium

**A real-time leaderboard system.** Five server-authoritative games feed per-game and cross-game
leaderboards built on Redis sorted sets, with ranks that update in the browser as scores land.

Built in Go against the [roadmap.sh real-time leaderboard brief](https://roadmap.sh/projects/realtime-leaderboard-system).

> **Status:** in progress. Phases 0–3 of 10 complete — configuration, logging, the HTTP error
> envelope, health probes, a container stack, persistence with migrations, authentication with
> Argon2id and refresh token rotation, and all five games as server-authoritative state machines.
> **280 tests, 80% coverage.** See [Roadmap](#roadmap).

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
| `PODIUM_JWT_SECRET` | dev key | Access token signing key. **Rejected in production** if left at the default or shorter than 32 characters. |
| `PODIUM_ACCESS_TOKEN_TTL` | `15m` | How long an access token stays valid — and how long a logout takes to fully bite. |
| `PODIUM_REFRESH_TOKEN_TTL` | `720h` | Session length. |
| `PODIUM_ARGON2_MEMORY_KIB` | `65536` | Password hashing memory cost, in KiB. |
| `PODIUM_LOGIN_MAX_PER_ACCOUNT` | `8` | Failed logins per account per window. |
| `PODIUM_TRUST_PROXY_IP` | `false` | Whether `X-Forwarded-For` may set the client IP. |

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

## The games

Five games, each a state machine on the server. **There is no endpoint that accepts a score** —
there is a test asserting `POST /v1/scores` returns 404, because the entire anti-forgery argument
rests on that absence.

| Slug | Game | Metric | Direction |
|---|---|---|---|
| `reaction` | Wait for a signal, respond. Five rounds. | milliseconds | lower |
| `math-sprint` | Arithmetic against a 30 second clock | correct answers | higher |
| `memory` | Repeat a growing sequence | sequence length | higher |
| `word-scramble` | Unscramble words against a 60 second clock | words solved | higher |
| `number-guess` | Find three numbers in 1–100 from higher/lower hints | guesses | lower |

```
POST /v1/games/{slug}/sessions   open a session
GET  /v1/sessions/{id}           current state
POST /v1/sessions/{id}/moves     play
POST /v1/sessions/{id}/finish    score it
```

### State and View

This split is the mechanism everything else depends on.

**State** is private, stored as `jsonb`, and holds the answers — the numbers to guess, the target
word, when the signal is due. **View** is the projection a client is allowed to see. Every engine
produces both, and only the View is ever serialised into a response.

The clearest demonstration is `number-guess`. Start a session and the client gets:

```json
{"round": 1, "rounds": 3, "low": 1, "high": 100, "guesses_used": 0, "hint": "", "done": false}
```

while the database holds `"secrets": [56, 16, 10]`. Two sessions with *different seeds* produce
**byte-identical views** — there is a test asserting exactly that, because it proves the view
carries no information about the answers at all.

### Determinism, and why it is hashed rather than sequential

Every session stores a `seed`, and all randomness derives from it:

```go
derive(seed, "math-a", index)   // sha256(seed || label || index)
```

Hashing rather than a sequential PRNG means any value can be recomputed **on its own**, without
having generated the ones before it. Given a finished session's seed, the 40th question can be
regenerated directly to audit what the player was actually asked. A sequential generator would
require replaying the whole stream, and would break the moment the order of calls changed.

### Deadlines belong to the server

Timed games store `deadline_at` and check it against the server's clock on **every move**. A
client that stops calling home simply stops scoring; there is no client-side timer to tamper with
and no "time remaining" the client is trusted to report.

Sessions that end early are charged for what was never played. Quitting `number-guess` after one
guess scores the same as playing all three rounds badly, and one excellent `reaction` round
followed by quitting is charged 500 ms for each of the four missing rounds. Without this,
the optimal strategy on every lower-is-better game is to quit the moment you get lucky.

### Reaction time and the held response

`reaction` has a problem the others don't: the client must not know when the signal is due, or it
schedules a tap for that instant and posts a perfect score.

So the response *is* the signal. `{"action":"arm"}` returns nothing for 1.2–3 seconds; the moment
it arrives is "go". The engine cannot sleep — it is a pure function — so it returns
`Outcome{HoldUntil: t}` and the session service performs the wait. Verified live: `arm` took
2140 ms, and `go_at` was in the database but absent from the response.

Crucially the wait happens **after the transaction commits**. Sleeping inside it would hold the
session row's lock for three seconds and stall every other request touching it.

**The honest caveat:** this measures reaction time *plus* one network round trip, because the
server timestamps both ends. Measuring on the client would be more accurate and completely
forgeable. Given the choice, this project takes the number it can actually defend.

### What this does and does not prevent

It prevents **score forgery**. A client cannot submit a score, cannot see the answers, and cannot
extend its own deadline.

It does **not** prevent automation. A script can binary-search `number-guess` optimally, compute
arithmetic instantly, and echo a `memory` sequence perfectly. Two partial mitigations exist —
plausibility floors (a sub-80 ms reaction is charged as a foul rather than accepted as a record)
and per-account rate limits — but a determined bot will still outplay a human. Genuinely stopping
that needs proof-of-humanity, which is a different project.

`memory` is the honest exception even to the forgery claim: the game *is* remembering a sequence,
so the sequence must be shown. The score is still computed by the server, but the answer is
unavoidably in the client's hands. There is a test named after this so nobody later assumes it
behaves like the others.

### Sessions

One live session per game per user. Starting a second `memory` session marks the first
`abandoned`, which bounds accumulation without needing a cleanup job. Sessions for *different*
games coexist.

`Move` locks its session row `FOR UPDATE` for the duration. Without that lock two concurrent moves
both read the same state and the second write silently discards the first — which in `math-sprint`
means answering twice and being credited once.

`Finish` is idempotent, and the `UNIQUE` constraint on `score_events.session_id` is what makes
that true under concurrency rather than just in the happy path. A test fires eight simultaneous
finishes and asserts exactly one score row.

Another user's session returns **404, not 403**. A distinct status would confirm the id exists,
which turns the endpoint into a probe for other people's sessions.

### Scoring

Each game maps its raw metric onto a shared **0–10,000 points** scale. This is needed for the
cross-game leaderboard to add anything together at all: 210 milliseconds and 14 solved anagrams
have no common unit until one is invented. It also means Redis only ever stores a
higher-is-better number, so a sorted set needs no special case for games where lower wins.

The anchors are a judgement call, documented rather than hidden:

| Game | 0 points | 10,000 points |
|---|---|---|
| `reaction` | 500 ms mean | 180 ms mean |
| `math-sprint` | 0 correct | 30 correct |
| `memory` | length 0 | length 12 |
| `word-scramble` | 0 words | 15 words |
| `number-guess` | 21 guesses | 9 guesses |

`number-guess` is the most luck-dependent of the five — a fortunate first guess beats a perfect
binary search. Three rounds instead of one reduces the variance; it does not remove it.

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

## Authentication

```
POST /v1/auth/register   create an account and sign in
POST /v1/auth/login      sign in with username or email
POST /v1/auth/refresh    exchange a refresh token for a new pair
POST /v1/auth/logout     revoke the session
GET  /v1/me              the authenticated account
```

```bash
curl -X POST localhost:8080/v1/auth/register -H 'content-type: application/json' \
  -d '{"username":"saeem","email":"saeem@example.com","password":"correct horse battery"}'
```

```json
{
  "user": {"username": "saeem", "email": "saeem@example.com", "created_at": "..."},
  "tokens": {
    "access_token": "eyJhbGciOiJIUzI1NiIs...",
    "token_type": "Bearer",
    "expires_in": 900,
    "refresh_token": "kQ8vX...",
    "refresh_expires_in": 2592000
  }
}
```

Note what is **not** there: no numeric id. Nothing outside the server needs it, and
[keeping it internal](#user-ids-are-bigint-not-uuids) is what makes the compact Redis identifier
safe to use.

### Passwords: Argon2id, with the cost inside the hash

```
$argon2id$v=19$m=65536,t=2,p=2$<salt>$<hash>
```

Argon2id is memory-hard, which is the property that matters: bcrypt's work factor costs an
attacker CPU, while Argon2's memory cost denies them the GPU and ASIC parallelism that makes
offline cracking cheap. The default here is 64 MiB per hash.

Every hash carries its own salt **and its own cost parameters**. Verification reads the cost from
the hash rather than from configuration, which is what makes the cost changeable: raising it in
config would otherwise invalidate every existing password at once. Instead a login against a hash
stored at an older cost succeeds and then transparently rehashes at the new one, so the fleet
migrates itself as people sign in. There is a test that raises the cost and asserts the stored
hash changes.

### Login reveals nothing about which accounts exist

Two things would otherwise leak it. Both are handled:

- **The message.** "No such user" and "wrong password" return the same `401` and the same body. A
  test asserts the two are byte-identical.
- **The timing.** Returning early for an unknown username makes that case measurably faster than a
  real password check — a timing oracle for valid usernames. So when the lookup misses, Podium
  verifies the password against a decoy hash computed at startup and throws the result away. The
  work is wasted on purpose.

### Access tokens

HS256 JWTs, 15 minute lifetime, carrying only the user id, issuer, expiry and a token id.

HS256 rather than RS256 because one service both signs and verifies. Asymmetric keys buy the
ability to let a third party verify without being able to mint, and there is no third party here —
it would add key distribution and rotation for no gain.

The algorithm is **pinned** at verification:

```go
jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()})
```

Without that line, a token whose header says `"alg": "none"` is accepted with no signature at all,
and an RS256 token can be verified against the HMAC secret treated as a public key. That is the
classic JWT algorithm confusion attack, and both variants have their own test here.

### Refresh tokens: rotation with theft detection

Every refresh token is **single use**. Spending one issues a new one, and every token descended
from a single login shares a **family id**.

```
login ──► r1
          r1 spent ──► r2
                       r2 spent ──► r3   ← normal rotation
```

That structure is what makes stolen credentials detectable. Suppose an attacker copies `r1`:

```
attacker spends r1 ──► r2'          Podium cannot tell who this is
victim   spends r1 ──► r1 is already used
                        │
                        └──► the ENTIRE family is revoked; r2' dies too
```

Presenting an already-used token means two parties hold it. Podium has no way to know which one is
the legitimate user, so it ends the session for both and forces a fresh login — where the password
is the deciding factor again. The alternative, ignoring the replay, silently lets a thief ride
along indefinitely.

The claim is a single conditional `UPDATE`:

```sql
UPDATE refresh_tokens SET used_at = now()
WHERE token_hash = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
RETURNING *;
```

One statement, so it is atomic. Of two concurrent refreshes with the same token exactly one wins;
the loser finds `used_at` already set and takes the theft path. Checking-then-updating in two
statements would let both succeed under load and make the detection useless precisely when it
matters.

**The honest tradeoff:** a legitimate client that submits the same refresh token twice — two tabs
racing, a retry after a lost response — is indistinguishable from a thief and gets logged out.
That is the accepted cost of strict detection. Softening it with a grace window would reopen
exactly the hole it exists to close.

Tokens are stored as **SHA-256 hashes**, not Argon2. A refresh token is 256 bits from a CSPRNG, so
there is no low-entropy guess space for a slow hash to protect — Argon2 would add its full cost to
every refresh and buy nothing. Hashing at all is what matters: a leaked database is then not a set
of live sessions.

**What logout cannot do.** Revoking the family kills the refresh chain, but a signed JWT is
verifiable without touching the database, so an already-issued access token keeps working until it
expires. That is inherent to stateless tokens; the 15 minute TTL is the bound on the exposure.
Checking a revocation list on every request would trade it away for a database read per call.
There is a test asserting this behaviour, so it can't change silently.

### Login throttling

Two counters, both required:

| Key | Default | Attack it stops |
|---|---|---|
| per IP | 20 / 15 min | one host spraying a password across many accounts |
| per account | 8 / 15 min | a botnet grinding one account from many addresses |

Either alone leaves the other attack wide open. A successful login clears both, so someone who
mistypes twice and then gets it right is not left with a counter creeping towards a lockout.

The counter is a single Lua script rather than `INCR` followed by `PEXPIRE`, because those are two
round trips: a client that dies between them leaves a counter with **no expiry**, and that account
is then locked out by something nothing will ever clear.

Windows are fixed, not sliding, so a caller can get up to 2× the limit across a boundary. For
making credential stuffing expensive that is a fine price for one counter and one round trip.

If Redis is unreachable the limiter **fails open** — it logs and allows the request. Login
availability matters more than the throttle, and the password check behind it is still doing the
actual work. Failing closed would turn a cache outage into a total lockout.

### Trusting proxy headers is opt-in

`PODIUM_TRUST_PROXY_IP` defaults to `false`, and while it is false `X-Forwarded-For` is ignored
entirely. Any client can set that header, so honouring it unconditionally would let an attacker
defeat per-IP throttling by sending a fresh value on every request. Behind a real proxy you must
turn it on — and in production Podium logs a warning when it is off, because then every request
appears to come from the proxy and the per-IP limit silently protects nothing.

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
| 2 | Auth — register/login/refresh, Argon2id, JWT, refresh rotation with reuse detection | done |
| 3 | Games and sessions — five engines, server-side deadlines, seeded and replayable | done |
| 4 | Leaderboards — sorted sets, atomic submission in Lua, top-N, rank, neighbours | next |
| 5 | Consistency — projection cursor, self-healing drift, rebuild from Postgres | |
| 6 | Realtime — WebSockets, Pub/Sub fan-out, coalescing, slow-client eviction | |
| 7 | Reports and rate limiting — hot and cold period reports, token buckets | |
| 8 | Demo UI — play all five games, watch ranks reorder live | |
| 9 | Production — metrics, OpenAPI, load test results, deployment on two instances | |

## Layout

```
cmd/api/                     the HTTP server
cmd/migrate/                 one-shot schema migration job
internal/auth/               Argon2id, JWTs, refresh rotation, no HTTP knowledge
internal/games/              the five engines, pure state machines with no I/O
internal/session/            session lifecycle, the only place a score is written
internal/config/             environment configuration, validated at boot
internal/httpapi/            router, middleware, error envelope, handlers, probes
internal/ratelimit/          Redis fixed-window counters
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
