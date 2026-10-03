-- Per-app environment variables. The deployer snapshots them into an
-- immutable Kubernetes Secret at deploy time, so changing a value only
-- affects deployments created afterwards.
CREATE TABLE app_env (
    app_id     BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    key        TEXT NOT NULL CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]{0,254}$'),
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, key)
);
