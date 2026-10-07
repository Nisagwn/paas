-- Faz 22: managed databases (Postgres / Redis add-ons) and a database copy
-- per preview branch.
--
-- 1. Add-ons. An app has 0..n add-ons; each runs as a StatefulSet with its
--    own PersistentVolumeClaim in the app's namespace (internal/deploy/
--    addons.go). The control plane is the source of truth for the
--    credentials: they are sealed with the env keyring (store/envcrypt.go,
--    key_id NULL = plaintext) and applied to a Secret by the reconcile loop.
--
--      provisioning ──(pod ready)──► ready ──(DELETE)──► deleting ──► row removed
--           │  ▲                       │
--           ▼  │ (pod ready again)     └─ a crash keeps "ready" (variables
--         failed (not ready in time)      stay injected); message tells why
--
--    Postgres: the bootstrap superuser "postgres" (admin password, never
--    injected) and an application role "app" owning the database "app";
--    deployments get the application role. Redis: requirepass + AOF.
CREATE TABLE addons (
    id               BIGSERIAL PRIMARY KEY,
    app_id           BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK (kind IN ('postgres', 'redis')),
    -- DNS label: objects are addon-<name>; also the prefix of the variables
    -- of a non-default name (<NAME>_DATABASE_URL).
    name             TEXT NOT NULL CHECK (name ~ '^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$'),
    plan             TEXT NOT NULL CHECK (plan IN ('hobby', 'standard', 'pro')),
    status           TEXT NOT NULL DEFAULT 'provisioning'
                     CHECK (status IN ('provisioning', 'ready', 'failed', 'deleting')),
    -- Last problem seen by the reconcile loop ('' = healthy).
    message          TEXT NOT NULL DEFAULT '',
    -- What a preview deployment connects to (Postgres only; Redis is shared):
    --   copy   : its own database, a copy of production (optionally anonymized)
    --   empty  : its own empty database
    --   shared : the production database itself
    preview_mode     TEXT NOT NULL DEFAULT 'copy' CHECK (preview_mode IN ('copy', 'empty', 'shared')),
    -- Column rules applied to every copy ("users.email: email", see
    -- internal/addons/anonymize.go) and optional raw SQL statements a member
    -- wrote, run after the rules inside the copy only.
    anonymize        JSONB NOT NULL DEFAULT '[]',
    anonymize_sql    TEXT NOT NULL DEFAULT '' CHECK (length(anonymize_sql) <= 16384),
    -- Daily logical backups kept (0 = no backups).
    backup_keep      INT NOT NULL DEFAULT 7 CHECK (backup_keep BETWEEN 0 AND 30),
    -- Sealed JSON {"admin": "...", "password": "..."}; next_secrets holds the
    -- password of a requested rotation until the reconcile loop applied it.
    secrets          TEXT NOT NULL,
    next_secrets     TEXT,
    key_id           TEXT,
    -- A rotation also redeploys what the aliases point at.
    rotate_redeploy  BOOLEAN NOT NULL DEFAULT false,
    -- Bumped by every rotation: rolls the Redis pod.
    secrets_version  INT NOT NULL DEFAULT 1,
    -- Lease of the reconcile loop (FOR UPDATE SKIP LOCKED, like rollouts).
    reconciled_at    TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at         TIMESTAMPTZ,
    UNIQUE (app_id, name),
    CHECK (kind = 'postgres' OR preview_mode = 'shared')
);
CREATE INDEX addons_reconcile_idx ON addons (reconciled_at);

-- 2. Database copies, one per (Postgres add-on, preview branch). The
--    database and its owner role are both named naming.BranchDatabase(branch)
--    ("preview_<slug>[_<hash>]"); the role's password is sealed like the
--    add-on's. The copy runs as a Kubernetes Job (pg_dump | pg_restore).
--
--      pending ──(Job created)──► copying ──► ready | failed
--      ready/failed ──(reset, or a new push after a failure)──► pending (generation + 1)
--      any ──(branch deleted, no live deployment left)──► deleting ──(DROP DATABASE)──► row removed
CREATE TABLE addon_branches (
    id            BIGSERIAL PRIMARY KEY,
    addon_id      BIGINT NOT NULL REFERENCES addons(id) ON DELETE CASCADE,
    branch        TEXT NOT NULL CHECK (length(branch) BETWEEN 1 AND 255),
    database      TEXT NOT NULL CHECK (database ~ '^[a-z_][a-z0-9_]{0,62}$'),
    -- What was asked (copy or empty); a copy above the size limit falls back
    -- to empty and says so in warning.
    mode          TEXT NOT NULL CHECK (mode IN ('copy', 'empty')),
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'copying', 'ready', 'failed', 'deleting')),
    error         TEXT NOT NULL DEFAULT '',
    warning       TEXT NOT NULL DEFAULT '',
    -- When pg_dump started reading production: the data is as of this time.
    snapshot_at   TIMESTAMPTZ,
    size_bytes    BIGINT,
    password      TEXT NOT NULL,
    key_id        TEXT,
    -- Bumped by every re-copy; the Job name carries it.
    generation    INT NOT NULL DEFAULT 0,
    attempts      INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (addon_id, branch),
    UNIQUE (addon_id, database)
);

-- 3. Logical backups (pg_dump -Fc into the add-on's backup volume). Rows are
--    created by a manual request (pending, then the reconcile loop starts the
--    Job) or imported from the Jobs of the daily CronJob; job is the Job name
--    and the file is <job>.dump. restore_* track a restore into production.
CREATE TABLE addon_backups (
    id                  BIGSERIAL PRIMARY KEY,
    addon_id            BIGINT NOT NULL REFERENCES addons(id) ON DELETE CASCADE,
    job                 TEXT NOT NULL CHECK (job ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    trigger             TEXT NOT NULL CHECK (trigger IN ('scheduled', 'manual')),
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'expired')),
    error               TEXT NOT NULL DEFAULT '',
    size_bytes          BIGINT,
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ,
    restore_status      TEXT NOT NULL DEFAULT ''
                        CHECK (restore_status IN ('', 'pending', 'running', 'succeeded', 'failed')),
    restore_error       TEXT NOT NULL DEFAULT '',
    restore_by          TEXT NOT NULL DEFAULT '',
    restore_started_at  TIMESTAMPTZ,
    restore_finished_at TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (addon_id, job)
);
CREATE INDEX addon_backups_addon_idx ON addon_backups (addon_id, id DESC);
-- One restore at a time per add-on.
CREATE UNIQUE INDEX addon_backups_one_restore ON addon_backups (addon_id)
    WHERE restore_status IN ('pending', 'running');
