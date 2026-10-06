-- Faz 21: gradual rollouts (canary) and automatic rollback.
--
-- 1. Per-app rollout settings. An app without a row behaves as before
--    (mode "instant": a ready production deployment takes the production
--    alias at once).
--      instant : today's behaviour
--      guarded : instant switch, then the new production is watched for
--                guard_seconds and rolled back automatically if it degrades
--      canary  : the new production first gets steps[0] % of the traffic,
--                then steps[1] %, ...; the production alias moves only at 100 %
--    Thresholds are compared with the request metrics of Faz 19 (one row
--    per deployment and minute), hence the 2 minute minimum step.
CREATE TABLE rollout_settings (
    app_id                 BIGINT PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    mode                   TEXT NOT NULL DEFAULT 'instant' CHECK (mode IN ('instant', 'guarded', 'canary')),
    -- Canary weights in percent, strictly increasing, ending with 100.
    steps                  INT[] NOT NULL DEFAULT '{10,50,100}',
    step_seconds           INT NOT NULL DEFAULT 300 CHECK (step_seconds BETWEEN 120 AND 86400),
    -- 5xx rate of the new deployment: absolute ceiling (percent) and the
    -- most it may exceed the current production's rate (percentage points).
    max_error_pct          DOUBLE PRECISION NOT NULL DEFAULT 5 CHECK (max_error_pct > 0 AND max_error_pct <= 100),
    max_error_increase_pct DOUBLE PRECISION NOT NULL DEFAULT 2 CHECK (max_error_increase_pct >= 0 AND max_error_increase_pct <= 100),
    -- p95 latency: absolute ceiling in ms (0 = off) and factor of the
    -- current production's p95 (0 = off).
    max_p95_ms             INT NOT NULL DEFAULT 0 CHECK (max_p95_ms >= 0),
    max_p95_factor         DOUBLE PRECISION NOT NULL DEFAULT 2 CHECK (max_p95_factor = 0 OR max_p95_factor >= 1),
    -- Requests the new deployment must serve in a step before its rates
    -- count. Below it a step advances on time unless there is evidence of
    -- failure (internal/rollout).
    min_requests           INT NOT NULL DEFAULT 50 CHECK (min_requests >= 1),
    guard_seconds          INT NOT NULL DEFAULT 600 CHECK (guard_seconds BETWEEN 120 AND 86400),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 2. Rollouts: one row per canary or guard run.
--
--   running ──(step passed)──► running (next step) ──(100 %)──► promoted
--      │  ▲
--      ▼  │ resume
--    paused
--   running/paused ──(thresholds broken)──► rolled_back
--   running/paused ──(user abort, manual rollback/promote)──► aborted
--   running/paused ──(newer production deployment)──► superseded
--
-- At most one active (running or paused) rollout per app. The settings in
-- force when it started are kept in "settings", so changing them does not
-- alter a running rollout. verdicts is the per-step decision log shown in
-- the UI: [{"at", "step", "weight", "action", "reason", "canary", "baseline"}].
CREATE TABLE rollouts (
    id                  BIGSERIAL PRIMARY KEY,
    app_id              BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    from_deployment_id  BIGINT REFERENCES deployments(id) ON DELETE SET NULL,
    to_deployment_id    BIGINT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    mode                TEXT NOT NULL CHECK (mode IN ('canary', 'guarded')),
    step                INT NOT NULL DEFAULT 0 CHECK (step >= 0),
    -- Share of the production traffic the new deployment gets (percent).
    weight              INT NOT NULL CHECK (weight BETWEEN 0 AND 100),
    state               TEXT NOT NULL DEFAULT 'running'
                        CHECK (state IN ('running', 'paused', 'promoted', 'rolled_back', 'aborted', 'superseded')),
    reason              TEXT NOT NULL DEFAULT '',
    settings            JSONB NOT NULL,
    verdicts            JSONB NOT NULL DEFAULT '[]',
    step_started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Lease of the controller loop: a replica claims a rollout for one
    -- evaluation by moving evaluated_at (FOR UPDATE SKIP LOCKED).
    evaluated_at        TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ
);

CREATE UNIQUE INDEX rollouts_one_active ON rollouts (app_id) WHERE state IN ('running', 'paused');
CREATE INDEX rollouts_app_idx ON rollouts (app_id, id DESC);
CREATE INDEX rollouts_to_idx ON rollouts (to_deployment_id);
