-- Faz 7: deployment retirement and crash recovery.
--
-- retired: the deployment no longer runs; its Kubernetes objects are (being)
-- deleted and it can never receive traffic again. Rows are kept for history.
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_status_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_status_check
    CHECK (status IN ('queued', 'building', 'deploying', 'ready', 'failed', 'retired'));

ALTER TABLE deployments
    -- Set when the deployment is retired, or while it is still in flight when
    -- its branch was deleted (it then ends as 'retired' instead of 'ready').
    ADD COLUMN retired_at    TIMESTAMPTZ,
    ADD COLUMN retire_reason TEXT NOT NULL DEFAULT '',
    -- Set once the Kubernetes objects of a retired or failed deployment are gone.
    ADD COLUMN cleaned_at    TIMESTAMPTZ,
    -- Claims so far; a worker's heartbeat is only valid for its own attempt.
    ADD COLUMN attempts      INT NOT NULL DEFAULT 0,
    -- Refreshed by the worker running the deployment. A stale heartbeat
    -- means the worker died, and the deployment is recovered.
    ADD COLUMN heartbeat_at  TIMESTAMPTZ;

-- Garbage collection and recovery scan these.
CREATE INDEX deployments_cleanup_idx ON deployments (app_id, id)
    WHERE status IN ('retired', 'failed') AND cleaned_at IS NULL;
CREATE INDEX deployments_inflight_idx ON deployments (id)
    WHERE status IN ('building', 'deploying');
CREATE INDEX aliases_deployment_idx ON aliases (deployment_id);
