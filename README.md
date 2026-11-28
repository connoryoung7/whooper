# whooper

Mirrors your entire WHOOP history (cycles, sleeps, recoveries, workouts, profile, body
measurements) into Postgres via the WHOOP Developer API v2, keeps it up to date on a
schedule, and tells you whether the copy is complete.

One binary, four commands:

| Command | What it does |
| --- | --- |
| `whooper serve` | Web server: WHOOP connect flow (`/auth/login`), completeness report (`/status`), `/health`. Default container command. |
| `whooper sync [--full]` | One sync pass, then exits. Run this on a schedule. |
| `whooper status [--json]` | Prints the completeness report; exits 1 if the history is incomplete. |
| `whooper migrate [status\|up\|down]` | Lists applied schema migrations (default), applies pending ones, or rolls back the newest. |

## How "all of my data, all time" is guaranteed

- **First sync is a full download.** Each collection is paged through with no `start`
  filter, which the WHOOP API defines as "no lower bound", until there is no `next_token`.
  A collection only counts as downloaded (`full_history_downloaded`) once a full pass finishes
  without an error. If one dies halfway, the next run starts the full pass over.
- **Incremental syncs can't leave gaps.** They re-read from *last successful sync − 14 days*
  (`SYNC_LOOKBACK_DAYS`), not from "now − N". So after downtime of any length, the next run
  catches up. The overlap also picks up records WHOOP scores or edits after the fact.
- **Periodic full re-downloads reconcile everything.** `sync --full` re-reads the whole
  history, updates anything that changed, and marks records that WHOOP no longer returns
  with `deleted_at`. Rows are never hard-deleted.
- **Nothing WHOOP returns is dropped.** Every record is stored verbatim in a `raw jsonb`
  column. A record that can't be parsed fails the sync loudly instead of being skipped.
- **`whooper status` / `/status` checks the result.** It reports INCOMPLETE if any of these
  hold:
  - no account is connected
  - a collection has never completed a full download
  - the last full download is older than 8 days, or the last sync is older than 24 hours
    (so a broken cron schedule shows up)
  - the last sync errored
  - records reference others that aren't stored (a recovery whose sleep or cycle is missing,
    or a sleep whose cycle is missing)

  Breaks in the chain of cycles, usually days the strap wasn't worn, are listed as
  warnings.
- **Runs are safe to overlap.** A Postgres advisory lock means a second run just skips.
  Token refreshes are serialized with a row lock, because WHOOP rotates refresh tokens and
  two refreshes at once would break each other.

## 1. Create a WHOOP developer app

At <https://developer-dashboard.whoop.com>, create an app:

- **Scopes:** all `read:*` scopes (profile, body_measurement, cycles, recovery, sleep,
  workout). whooper also requests `offline`, which is what allows unattended refreshes.
- **Redirect URL:** `https://<your-domain>/auth/callback`. For local development, also add
  `http://localhost:8080/auth/callback`.

Keep the client ID and secret for step 2.

## 2. Deploy on Dokploy

The app runs as one long-lived container (`whooper serve`). It handles the one-time WHOOP
login and serves `/status`. Dokploy schedules run `whooper sync` inside that same
container. The database is a Dokploy-managed Postgres in the same project.

### Database

1. In your Dokploy project, create a **PostgreSQL** database and deploy it.
2. Under *Internal Credentials*, copy the **Internal Connection URL**. It looks like
   `postgresql://<user>:<password>@<db-app-name>:5432/<database>` and can be used as-is.
3. Turn on **backups** (this needs an S3 destination configured in Dokploy). The WHOOP data
   can be downloaded again, but your body-measurement history and the WHOOP token exist
   only in this database.

### Application

1. Create an **Application** in the same project.
2. **Source:** the repository is public, so use the **Git** provider with
   `https://github.com/connoryoung7/whooper.git`, branch `main`, and no credentials. Use the
   **GitHub** provider instead if you want Dokploy to redeploy on every push.
3. **Build type:** **Dockerfile**, Dockerfile path `Dockerfile`, Docker context path `.`
4. **Environment:**
   ```
   DATABASE_URL=<internal connection URL from above>
   WHOOP_CLIENT_ID=<from the WHOOP developer app>
   WHOOP_CLIENT_SECRET=<from the WHOOP developer app>
   WHOOP_REDIRECT_URL=https://<your-domain>/auth/callback
   ADMIN_PASSWORD=<long random string>
   ```
   Optional: `PORT` (default `8080`) and `SYNC_LOOKBACK_DAYS` (default `14`).
5. **Domains:** add `<your-domain>` with container port **8080**, HTTPS on, and the
   Let's Encrypt certificate. The domain's DNS must point at your Dokploy server.
6. **Deploy.** The logs should show `applied migration 00001_initial_schema.sql`, then
   `listening on :8080`. The image's `HEALTHCHECK` polls `/health`.

### Connect WHOOP

Open `https://<your-domain>/auth/login`. When the browser asks for a username and
password, enter any username and `ADMIN_PASSWORD`. Approve access on WHOOP.

The callback stores the token and starts the first full download in the background. It
takes a few minutes, because requests are throttled to about 92 a minute to stay under
WHOOP's limit of 100. Watch progress at `/status`.

### Schedules

In the application's **Schedules** tab, add two **Application** schedules:

| Name | Cron | Command |
| --- | --- | --- |
| WHOOP sync | `0 * * * *` | `whooper sync` |
| WHOOP full reconcile | `30 3 * * *` | `whooper sync --full` |

Dokploy runs each one as `docker exec <container> bash -c "<command>"`. The default
**Bash** shell type works, because the image includes bash (`sh` works too). Set a
timezone if you want the cron times to be local rather than the server's.

Each run's log ends with the completeness report. A failed sync exits non-zero, so Dokploy
marks that run as failed. API usage is roughly 5–10 requests per hourly run and about 60
per year of history per full run, well under WHOOP's 10,000 a day.

### Check it

After the first full download, `https://<your-domain>/status` should report
`"complete": true`. `whooper status` prints the same report from the Dokploy terminal and
exits 1 if anything is missing. You can also add it as a daily schedule.

### Troubleshooting

- **The callback fails after approving on WHOOP:** the application logs show the error
  WHOOP returned. The usual cause is a `WHOOP_REDIRECT_URL` that doesn't exactly match the
  redirect URL registered on the WHOOP app (scheme, domain and path).
- **"refresh token was rejected":** WHOOP no longer accepts the stored token, for example
  because you revoked access in the WHOOP app. Visit `/auth/login` again.
- **Connecting a different WHOOP account:** the server refuses with HTTP 409. One
  deployment mirrors one WHOOP user and won't mix another user's data into it.
- **The build fails with `go.mod requires go >= …`:** a dependency update raised the Go
  version in `go.mod`. Raise the `golang:` image tag in the `Dockerfile` to match.

## Local development

```bash
cp .env.example .env      # fill in WHOOP_CLIENT_ID / SECRET / ADMIN_PASSWORD
make docker-run           # Postgres + api + cron from docker-compose.yml
                          # → http://localhost:8080/auth/login
make test                 # integration tests need Docker (testcontainers)
```

`docker-compose.yml` runs three services: `psql_bp` (Postgres), `api` (`whooper serve`),
and `cron`, which runs `whooper sync` hourly and `whooper sync --full` at 03:30, the same
schedules as in Dokploy. Cron times use `TZ` from `.env` (default UTC). Follow the syncs
with `docker compose logs -f cron`, and run one by hand with
`docker compose exec api whooper sync`.

To run the Go code directly instead, start only the database and use the make targets:

```bash
docker compose up -d psql_bp
make run                  # whooper serve → http://localhost:8080/auth/login
make sync                 # or: make sync FULL=1
make status
```

## Database migrations

The schema is managed with [goose](https://github.com/pressly/goose). Migrations are
numbered SQL files in `internal/database/migrations/`, built into the binary. Every command
applies pending migrations when it starts, so deploying a new version is enough. A
Postgres advisory lock makes a web server and a scheduled sync that start together take
turns. Applied versions are recorded in the `goose_db_version` table.

To change the schema:

```bash
make migration name=add_workout_zone_columns   # creates internal/database/migrations/00002_….sql
```

Write the `-- +goose Up` and `-- +goose Down` sections. `make itest` runs the migrations
against a real Postgres. Never edit a migration that has already been deployed; add a new
one instead.

On the server, `whooper migrate status` lists what's applied (run it from the Dokploy
terminal). `whooper migrate down` rolls back the newest migration. It refuses to roll back
`00001`, because that would drop every table.

## Data model

Collection tables are `whoop_cycle`, `whoop_sleep`, `whoop_recovery` and `whoop_workout`.
Each has:

- the full API record in `raw`
- extracted columns: ids, `start_at`, `end_at`, `updated_at`, `score_state`, plus
  `cycle_id`/`sleep_id`/`nap`/`sport_name` where they apply
- bookkeeping: `first_seen_at`, `last_seen_at`, `deleted_at`

The other tables:

- `whoop_profile` and `whoop_body_measurement`: the body measurement gets a new row each
  time it changes, since WHOOP only exposes the current value.
- `whoop_sync_state` and `whoop_sync_run`: sync bookkeeping.
- `whoop_token`: the OAuth token pair.

```sql
-- Daily recovery, HRV and strain
SELECT c.start_at::date                               AS day,
       (r.raw->'score'->>'recovery_score')::numeric   AS recovery,
       (r.raw->'score'->>'hrv_rmssd_milli')::numeric  AS hrv_ms,
       (c.raw->'score'->>'strain')::numeric           AS strain
FROM whoop_cycle c
LEFT JOIN whoop_recovery r ON r.cycle_id = c.id AND r.deleted_at IS NULL
WHERE c.deleted_at IS NULL
ORDER BY day DESC;
```

## Limits

- "All time" means everything the WHOOP API returns. To cross-check, request WHOOP's own
  data export from the app and compare its record counts with the `status` output.
- Body measurement history only starts when whooper was first deployed.
