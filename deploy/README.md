# Deploying Podium

> **Status: written, not run.** The configuration here has never been applied to a real Fly
> account. Everything else in this repository was verified by executing it; this was not, and it
> would be dishonest to present it otherwise. Treat it as a starting point that will need at least
> one round of correction.

## Why Fly

The interesting property of this system only appears on more than one instance: a score submitted
to instance A has to reach a WebSocket held by instance B. `fly scale count 2` makes that a single
command, which is the whole reason this target was chosen over something simpler.

## Once

```bash
fly launch --no-deploy --copy-config --config deploy/fly.toml

fly postgres create --name podium-db --region lhr
fly postgres attach podium-db            # sets DATABASE_URL

fly redis create --name podium-redis --region lhr
# copy the connection string it prints

fly secrets set \
  PODIUM_JWT_SECRET="$(openssl rand -base64 48)" \
  PODIUM_DATABASE_URL="postgres://..." \
  PODIUM_REDIS_URL="redis://..."
```

`PODIUM_JWT_SECRET` has no usable default in production - the API refuses to start if it is left
at the development value or is shorter than 32 characters. That is deliberate: a signing key that
silently falls back to a published constant is worse than no signing at all.

## Every deploy

```bash
fly deploy --config deploy/fly.toml
```

`release_command = "migrate"` runs the schema migration once, before any machine takes traffic.
Migrating from the API's own startup would have every replica racing on the version table.

## Proving the fan-out

```bash
fly scale count 2 --process-group app
fly status
```

Then open the demo page in two browsers and play in one. Both boards move, because the change
travels over Redis Pub/Sub rather than through either process's memory.

## Operations

```bash
fly ssh console -C "admin status"                  # projection backlog and board sizes
fly ssh console -C "admin rebuild-leaderboards"    # rebuild every board from Postgres
fly ssh console -C "admin materialise-reports"     # freeze windows that have closed
```

## Things to check first

- **Machines must not stop.** `auto_stop_machines = false` is set because a WebSocket is held for
  as long as a page is open; stopping a machine under one disconnects every watcher on it.
- **`PODIUM_TRUST_PROXY_IP = "true"`** because Fly terminates TLS and forwards. Without it every
  request appears to come from the proxy and per-IP rate limiting protects nothing. It is only
  safe *because* a proxy sits in front - setting it on a directly exposed deployment would let any
  client spoof its own address.
- **Redis needs no persistence.** Everything it holds is derived from Postgres and can be rebuilt
  with one command, so paying for durability there buys nothing.
