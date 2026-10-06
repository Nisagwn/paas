-- Faz 20: process types (web / worker / cron).
--
-- 1. The process set a deployment was built with (internal/process.Set as
--    JSON): the builder reads paas.yaml or the Procfile of the commit it
--    builds, so the set is as immutable as the image. A deployment without
--    a row (built before Faz 20, or by a builder that does not detect
--    processes) runs its web process only. Redeploys and promotions that
--    reuse an image copy the row (store.CopyDeployment).
CREATE TABLE deployment_processes (
    deployment_id BIGINT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    spec          JSONB NOT NULL,
    -- Denormalized from spec for the alias router: a deployment without a
    -- web process gets no Ingress.
    has_web       BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 2. Per-app replica overrides set from the UI, API or CLI. They win over
--    the replicas in paas.yaml and apply to the production deployment;
--    a deployment that does not have the process ignores the row.
CREATE TABLE app_process_scale (
    app_id     BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    process    TEXT NOT NULL CHECK (process ~ '^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$' AND process <> 'web'),
    replicas   INT NOT NULL CHECK (replicas BETWEEN 0 AND 10),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (app_id, process)
);
