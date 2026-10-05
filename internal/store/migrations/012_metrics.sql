-- Faz 19: request analytics. The collector (internal/analytics) scrapes
-- Traefik's per-service counters every minute and stores the per-deployment
-- deltas here, one row per deployment and minute that saw traffic (idle
-- minutes have no row). Rows older than the retention (7 days) are deleted.
CREATE TABLE request_metrics (
    deployment_id  BIGINT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    app_id         BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    minute         TIMESTAMPTZ NOT NULL,
    requests       BIGINT NOT NULL DEFAULT 0,
    status_2xx     BIGINT NOT NULL DEFAULT 0,
    status_3xx     BIGINT NOT NULL DEFAULT 0,
    status_4xx     BIGINT NOT NULL DEFAULT 0,
    status_5xx     BIGINT NOT NULL DEFAULT 0,
    -- Response time histogram (seconds), when Traefik exports one:
    -- duration_buckets maps an upper bound ("0.1", "+Inf") to the cumulative
    -- count of this minute.
    duration_sum     DOUBLE PRECISION NOT NULL DEFAULT 0,
    duration_count   BIGINT NOT NULL DEFAULT 0,
    duration_buckets JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (deployment_id, minute)
);

CREATE INDEX request_metrics_app_minute ON request_metrics (app_id, minute);
CREATE INDEX request_metrics_minute ON request_metrics (minute);
