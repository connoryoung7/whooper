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

1. **Database:** create a Postgres database in your Dokploy project. From its
   *Internal Credentials*, copy the **Internal Connection URL**. Turn on scheduled
   **backups** for it, since this is your copy of the data.
2. **Application:** create an Application from this repository.
   - Build type **Dockerfile**, Dockerfile path `Dockerfile`, context `.`
   - **Environment:**
     ```
     DATABASE_URL=<internal connection URL>
     WHOOP_CLIENT_ID=...
     WHOOP_CLIENT_SECRET=...
     WHOOP_REDIRECT_URL=https://<your-domain>/auth/callback
     ADMIN_PASSWORD=<long random string>
     PORT=8080
     ```
   - **Domains:** add `<your-domain>`, container port `8080`, HTTPS on, certificate
     Let's Encrypt.
   - Deploy. The image has a `HEALTHCHECK` on `/health`.
3. **Connect WHOOP:** open `https://<your-domain>/auth/login`. Your browser will ask for a
   username and password: use any username and `ADMIN_PASSWORD`. Approve access on WHOOP.
   The callback stores the token and starts the first full download in the background.
   That download takes a few minutes, because requests are throttled to about 92 a minute
   to stay under WHOOP's limit of 100.
4. **Schedules:** in the application's **Schedules** tab, add *Application* jobs. Dokploy
   runs these with `docker exec` inside the running container:

   | Name | Cron | Command |
   | --- | --- | --- |
   | WHOOP sync | `0 * * * *` | `whooper sync` |
   | WHOOP full reconcile | `30 3 * * *` | `whooper sync --full` |

   Each run's log ends with the completeness report, and the exit code is non-zero if the
   sync failed. API usage is roughly 5–10 requests per hourly run and about 60 per year of
   history per full run, well under WHOOP's 10,000/day.
5. **Check it:** open `https://<your-domain>/status` (JSON), or run
   `whooper status` from the Dokploy terminal or as a schedule.

If the report says the refresh token was rejected (for example, you revoked access in the
WHOOP app), visit `/auth/login` again. A deployment refuses to connect a different WHOOP
user than the one whose data it already holds.

## Local development

```bash
cp .env.example .env      # fill in WHOOP_CLIENT_ID / SECRET / ADMIN_PASSWORD
make docker-run           # Postgres from docker-compose.yml
make run                  # whooper serve → http://localhost:8080/auth/login
make sync                 # or: make sync FULL=1
make status
make test                 # integration tests need Docker (testcontainers)
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
