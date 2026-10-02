CREATE TABLE apps (
    id                BIGSERIAL PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{1,30}$'),
    repo_full_name    TEXT NOT NULL UNIQUE,           -- "owner/repo"
    production_branch TEXT NOT NULL DEFAULT 'main',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deployments (
    id             BIGSERIAL PRIMARY KEY,
    app_id         BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    commit_sha     TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    branch         TEXT NOT NULL,
    commit_message TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued', 'building', 'deploying', 'ready', 'failed')),
    error          TEXT NOT NULL DEFAULT '',
    image          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    -- A commit is deployed once per app: deployments are immutable.
    UNIQUE (app_id, commit_sha)
);

-- The worker queue scans this index.
CREATE INDEX deployments_queued_idx ON deployments (id) WHERE status = 'queued';
CREATE INDEX deployments_app_idx ON deployments (app_id, id DESC);

CREATE TABLE aliases (
    id            BIGSERIAL PRIMARY KEY,
    app_id        BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    hostname      TEXT NOT NULL UNIQUE,
    kind          TEXT NOT NULL CHECK (kind IN ('production', 'preview')),
    branch        TEXT NOT NULL,
    deployment_id BIGINT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deployment_logs (
    id            BIGSERIAL PRIMARY KEY,
    deployment_id BIGINT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    ts            TIMESTAMPTZ NOT NULL DEFAULT now(),
    line          TEXT NOT NULL
);

CREATE INDEX deployment_logs_dep_idx ON deployment_logs (deployment_id, id);
