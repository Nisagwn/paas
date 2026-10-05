-- Faz 17: environments and deploy controls (Vercel parity).
--
-- 1. Environment-scoped variables. Every app_env row has a target:
--      all        : both environments (every row written before Faz 17, and
--                   writes through the API without a target)
--      production : deployments of the production branch (and promotions)
--      preview    : every other deployment; git_branch optionally narrows a
--                   preview row to one branch
--    Resolution, most specific wins:
--      production : all < production
--      preview    : all < preview < preview + git_branch
--    Existing rows become "all" without being rewritten: the encryption AAD
--    of an "all" row without a branch is the Faz 10 AAD (app id + name), so
--    their ciphertext stays valid. Rows of the other targets bind target and
--    branch into the AAD (store/envcrypt.go).
ALTER TABLE app_env ADD COLUMN target TEXT NOT NULL DEFAULT 'all'
    CHECK (target IN ('all', 'production', 'preview'));
ALTER TABLE app_env ADD COLUMN git_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE app_env ADD CONSTRAINT app_env_branch_preview
    CHECK (git_branch = '' OR target = 'preview');
ALTER TABLE app_env DROP CONSTRAINT app_env_pkey;
ALTER TABLE app_env ADD PRIMARY KEY (app_id, key, target, git_branch);

-- 2. Deployments get the environment they run in, where they came from and
-- a generation. A commit's first deployment (generation 0) keeps its names
-- (d-<sha7>, <sha7>-<app>); a redeploy or promotion of the same commit is a
-- new immutable deployment with generation n (d-<sha7>-<n>, <sha7>-<n>-<app>)
-- and its own env Secret.
ALTER TABLE deployments
    ADD COLUMN target TEXT NOT NULL DEFAULT 'preview' CHECK (target IN ('production', 'preview')),
    ADD COLUMN origin TEXT NOT NULL DEFAULT 'git' CHECK (origin IN ('git', 'redeploy', 'promote', 'hook')),
    ADD COLUMN source_deployment_id BIGINT REFERENCES deployments(id) ON DELETE SET NULL,
    ADD COLUMN generation INT NOT NULL DEFAULT 0 CHECK (generation >= 0),
    -- Set by a cancel request while the deployment is building/deploying;
    -- the worker's heartbeat loop sees it and cancels the run.
    ADD COLUMN cancel_requested_at TIMESTAMPTZ;

UPDATE deployments d SET target = 'production'
FROM apps a WHERE a.id = d.app_id AND d.branch = a.production_branch;

ALTER TABLE deployments DROP CONSTRAINT deployments_app_id_commit_sha_key;
ALTER TABLE deployments ADD CONSTRAINT deployments_app_commit_generation_key
    UNIQUE (app_id, commit_sha, generation);

-- 3. "canceled": stopped by a user before it finished.
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS deployments_status_check;
ALTER TABLE deployments ADD CONSTRAINT deployments_status_check
    CHECK (status IN ('queued', 'building', 'deploying', 'ready', 'failed', 'retired', 'canceled'));
DROP INDEX IF EXISTS deployments_cleanup_idx;
CREATE INDEX deployments_cleanup_idx ON deployments (app_id, id)
    WHERE status IN ('retired', 'failed', 'canceled') AND cleaned_at IS NULL;

-- 4. Deploy hooks: secret URLs that deploy the head of a branch. Only the
-- SHA-256 of the token is stored; prefix helps users tell hooks apart.
CREATE TABLE deploy_hooks (
    id                BIGSERIAL PRIMARY KEY,
    app_id            BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name              TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    branch            TEXT NOT NULL CHECK (length(branch) BETWEEN 1 AND 255),
    token_hash        TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    prefix            TEXT NOT NULL,
    created_by        BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_triggered_at TIMESTAMPTZ
);
CREATE INDEX deploy_hooks_app_idx ON deploy_hooks (app_id);
