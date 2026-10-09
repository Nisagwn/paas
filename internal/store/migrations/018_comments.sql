-- Faz 25: preview comments (Vercel Comments). Reviewers pin a comment to a
-- point of a deployment's page; others reply, a member resolves it.
--
-- One table holds threads and replies: a thread has thread_id NULL and the
-- pin (path, x/y in percent of the iframe viewport, viewport width) and a
-- status; a reply points at its thread and has neither.
CREATE TABLE preview_comments (
    id             BIGSERIAL PRIMARY KEY,
    deployment_id  BIGINT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    app_id         BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    thread_id      BIGINT REFERENCES preview_comments(id) ON DELETE CASCADE,
    -- author_id is NULL for the admin token (no user); author_login is
    -- kept so a comment still shows who wrote it.
    author_id      BIGINT REFERENCES users(id) ON DELETE SET NULL,
    author_login   TEXT NOT NULL,
    body           TEXT NOT NULL CHECK (char_length(body) BETWEEN 1 AND 2000),
    -- Logins of team members mentioned with @login (canonical spelling).
    mentions       TEXT[] NOT NULL DEFAULT '{}',
    path           TEXT CHECK (path IS NULL OR (path LIKE '/%' AND char_length(path) <= 1024)),
    x_pct          DOUBLE PRECISION CHECK (x_pct IS NULL OR (x_pct >= 0 AND x_pct <= 100)),
    y_pct          DOUBLE PRECISION CHECK (y_pct IS NULL OR (y_pct >= 0 AND y_pct <= 100)),
    viewport_width INTEGER CHECK (viewport_width IS NULL OR (viewport_width BETWEEN 1 AND 10000)),
    status         TEXT CHECK (status IN ('open', 'resolved')),
    resolved_at    TIMESTAMPTZ,
    resolved_by    TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((thread_id IS NULL) = (status IS NOT NULL)),
    CHECK (thread_id IS NULL OR (path IS NULL AND x_pct IS NULL AND y_pct IS NULL AND viewport_width IS NULL)),
    CHECK ((x_pct IS NULL) = (y_pct IS NULL))
);
CREATE INDEX preview_comments_deployment_idx ON preview_comments (deployment_id) WHERE thread_id IS NULL;
CREATE INDEX preview_comments_app_idx ON preview_comments (app_id, status) WHERE thread_id IS NULL;
CREATE INDEX preview_comments_thread_idx ON preview_comments (thread_id) WHERE thread_id IS NOT NULL;
