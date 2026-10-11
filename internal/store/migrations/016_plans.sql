-- Faz 23: plans, limits and abuse protection for a public platform.
--
-- 1. Every team has a plan. Its limits come from the configuration
--    (PAAS_PLAN_<PLAN>_<LIMIT>, internal/plans); plan_limits holds the
--    per-team overrides an admin set ({"projects": 50, ...}; missing keys
--    follow the plan).
--      hobby     : default; the personal team of an open-signup user
--      pro       : assigned by an admin
--      unlimited : assigned by an admin; also every team that existed
--                  before this migration (created under the allowlists)
ALTER TABLE teams ADD COLUMN plan TEXT NOT NULL DEFAULT 'hobby'
    CHECK (plan IN ('hobby', 'pro', 'unlimited'));
ALTER TABLE teams ADD COLUMN plan_limits JSONB NOT NULL DEFAULT '{}'
    CHECK (jsonb_typeof(plan_limits) = 'object');
UPDATE teams SET plan = 'unlimited';

-- 2. Suspension by an admin. A suspended team cannot deploy or change
--    anything that costs resources; its apps are scaled to zero and kept.
--    A suspended user cannot sign in, and their tokens stop working.
ALTER TABLE teams ADD COLUMN suspended_at TIMESTAMPTZ;
ALTER TABLE teams ADD COLUMN suspended_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN suspended_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN suspended_reason TEXT NOT NULL DEFAULT '';

-- 3. Build minutes: seconds the build stage of a deployment ran (all
--    attempts). Deployments reusing an image record 0.
ALTER TABLE deployments ADD COLUMN build_seconds INTEGER NOT NULL DEFAULT 0
    CHECK (build_seconds >= 0);

-- Usage windows (deployments today, build minutes this month) and the
-- per-team count of running builds the fair queue reads.
CREATE INDEX deployments_created_idx ON deployments (created_at);
CREATE INDEX deployments_running_idx ON deployments (app_id) WHERE status IN ('building', 'deploying');
