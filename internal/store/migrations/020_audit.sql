-- Faz 27: audit log. One row per sensitive action; the browser terminal
-- writes one per session (action 'terminal'): who opened it, on which
-- deployment and pod, when it started and ended, and how (exit code of the
-- shell, or why it was closed). Keystrokes and output are never stored.
CREATE TABLE audit_log (
    id            BIGSERIAL PRIMARY KEY,
    action        TEXT NOT NULL,
    app_id        BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    deployment_id BIGINT REFERENCES deployments(id) ON DELETE SET NULL,
    -- NULL for the admin principal (legacy API token / dev token login).
    user_id       BIGINT REFERENCES users(id) ON DELETE SET NULL,
    actor         TEXT NOT NULL,
    process       TEXT NOT NULL DEFAULT '',
    pod           TEXT NOT NULL DEFAULT '',
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at      TIMESTAMPTZ,
    exit_code     INTEGER,
    -- exit, idle, max_duration, client_closed, no_shell, error, shutdown
    end_reason    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX audit_log_app_started ON audit_log (app_id, started_at DESC);
