-- Faz 28: deleting a project.
--
-- Deletion runs in three steps, each one safe to repeat after a crash
-- (internal/appdelete):
--
--   1. begin   (one transaction): an app_deletions row is written, the app
--              gets deleting_at (hidden from lists, pushes, hooks and the
--              API; nothing new is queued or claimed for it), queued
--              deployments are canceled and running ones are asked to stop
--              (the cancel machinery of Faz 17).
--   2. rows    (one transaction, once no deployment runs any more): the app
--              row is deleted; every table that references it cascades.
--   3. cluster: the namespace app-<name> (and any other namespace labelled
--              with the app) is deleted, plus retained volumes of its
--              claims; finished_at is set once the namespace is gone.
--
-- A new app may take the name once finished_at is set (store.CreateAppInTeam
-- checks it), so a new project never meets the old namespace.
ALTER TABLE apps ADD COLUMN deleting_at TIMESTAMPTZ;

CREATE TABLE app_deletions (
    id              BIGSERIAL PRIMARY KEY,
    -- The deleted app's id and name, kept as plain values: the app row is
    -- gone after step 2.
    app_id          BIGINT NOT NULL UNIQUE,
    app_name        TEXT NOT NULL,
    repo_full_name  TEXT NOT NULL,
    team_id         BIGINT REFERENCES teams(id) ON DELETE SET NULL,
    requested_by    TEXT NOT NULL,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    rows_deleted_at TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    -- Lease of the step runner (another replica skips a claimed row).
    claimed_until   TIMESTAMPTZ
);
CREATE INDEX app_deletions_pending_idx ON app_deletions (id) WHERE finished_at IS NULL;
CREATE INDEX app_deletions_name_idx ON app_deletions (app_name) WHERE finished_at IS NULL;

-- The audit log outlives the app: rows keep the app's name as text and
-- lose the reference (app_id NULL) when the app row is deleted.
ALTER TABLE audit_log ADD COLUMN app_name TEXT NOT NULL DEFAULT '';
UPDATE audit_log l SET app_name = a.name FROM apps a WHERE a.id = l.app_id;
ALTER TABLE audit_log ALTER COLUMN app_id DROP NOT NULL;
ALTER TABLE audit_log DROP CONSTRAINT audit_log_app_id_fkey;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_app_id_fkey
    FOREIGN KEY (app_id) REFERENCES apps(id) ON DELETE SET NULL;
