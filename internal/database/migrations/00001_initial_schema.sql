-- Initial schema.
--
-- The IF NOT EXISTS clauses let goose adopt a database created before migrations existed
-- (when this schema was applied on every start): this migration becomes a no-op there and
-- the database is simply recorded as being at version 1. Later migrations don't need them.

-- +goose Up
-- The single WHOOP OAuth token pair. Refresh tokens rotate, so this row changes on every refresh.
CREATE TABLE IF NOT EXISTS whoop_token (
    id            smallint    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    access_token  text        NOT NULL,
    refresh_token text        NOT NULL,
    expires_at    timestamptz NOT NULL,
    scope         text        NOT NULL DEFAULT '',
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- One-time OAuth state values for /auth/login → /auth/callback.
CREATE TABLE IF NOT EXISTS whoop_oauth_state (
    state      text        PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS whoop_profile (
    user_id    bigint      PRIMARY KEY,
    raw        jsonb       NOT NULL,
    fetched_at timestamptz NOT NULL
);

-- WHOOP only exposes the current body measurement, so keep a row each time it changes.
CREATE TABLE IF NOT EXISTS whoop_body_measurement (
    id         bigserial   PRIMARY KEY,
    raw        jsonb       NOT NULL,
    fetched_at timestamptz NOT NULL
);

-- Collections. `raw` is the full API record, so no field WHOOP returns is lost; the other
-- columns are extracted for indexing and integrity checks. `deleted_at` is set when a full
-- sync no longer sees the record (it was deleted in WHOOP); rows are never hard-deleted.

CREATE TABLE IF NOT EXISTS whoop_cycle (
    id            bigint      PRIMARY KEY,
    start_at      timestamptz NOT NULL,
    end_at        timestamptz,
    updated_at    timestamptz,
    score_state   text,
    raw           jsonb       NOT NULL,
    first_seen_at timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL,
    deleted_at    timestamptz
);
CREATE INDEX IF NOT EXISTS whoop_cycle_start_at_idx ON whoop_cycle (start_at);

CREATE TABLE IF NOT EXISTS whoop_sleep (
    id            uuid        PRIMARY KEY,
    cycle_id      bigint,
    nap           boolean     NOT NULL DEFAULT false,
    start_at      timestamptz NOT NULL,
    end_at        timestamptz,
    updated_at    timestamptz,
    score_state   text,
    raw           jsonb       NOT NULL,
    first_seen_at timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL,
    deleted_at    timestamptz
);
CREATE INDEX IF NOT EXISTS whoop_sleep_start_at_idx ON whoop_sleep (start_at);
CREATE INDEX IF NOT EXISTS whoop_sleep_cycle_id_idx ON whoop_sleep (cycle_id);

CREATE TABLE IF NOT EXISTS whoop_recovery (
    cycle_id      bigint      PRIMARY KEY,
    sleep_id      uuid,
    updated_at    timestamptz,
    score_state   text,
    raw           jsonb       NOT NULL,
    first_seen_at timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL,
    deleted_at    timestamptz
);
CREATE INDEX IF NOT EXISTS whoop_recovery_sleep_id_idx ON whoop_recovery (sleep_id);

CREATE TABLE IF NOT EXISTS whoop_workout (
    id            uuid        PRIMARY KEY,
    sport_name    text,
    start_at      timestamptz NOT NULL,
    end_at        timestamptz,
    updated_at    timestamptz,
    score_state   text,
    raw           jsonb       NOT NULL,
    first_seen_at timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL,
    deleted_at    timestamptz
);
CREATE INDEX IF NOT EXISTS whoop_workout_start_at_idx ON whoop_workout (start_at);

-- Per-collection sync bookkeeping. backfill_complete only becomes true after a sync has paged
-- through the collection's entire history without error.
CREATE TABLE IF NOT EXISTS whoop_sync_state (
    resource               text        PRIMARY KEY,
    backfill_complete      boolean     NOT NULL DEFAULT false,
    last_full_sync_at      timestamptz,
    last_full_sync_records integer,
    last_success_at        timestamptz,
    last_error             text,
    last_error_at          timestamptz
);

CREATE TABLE IF NOT EXISTS whoop_sync_run (
    id          bigserial   PRIMARY KEY,
    mode        text        NOT NULL,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz,
    status      text        NOT NULL DEFAULT 'running',
    requests    integer,
    error       text
);

-- +goose Down
DROP TABLE IF EXISTS whoop_sync_run;
DROP TABLE IF EXISTS whoop_sync_state;
DROP TABLE IF EXISTS whoop_workout;
DROP TABLE IF EXISTS whoop_recovery;
DROP TABLE IF EXISTS whoop_sleep;
DROP TABLE IF EXISTS whoop_cycle;
DROP TABLE IF EXISTS whoop_body_measurement;
DROP TABLE IF EXISTS whoop_profile;
DROP TABLE IF EXISTS whoop_oauth_state;
DROP TABLE IF EXISTS whoop_token;
