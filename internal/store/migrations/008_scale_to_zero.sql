-- Faz 11: scale idle deployments to zero.
--
-- Kubernetes is the source of truth for whether a deployment sleeps (its
-- replicas and Service); sleeping_since mirrors it for the API and the UI.
ALTER TABLE deployments ADD COLUMN sleeping_since TIMESTAMPTZ;

-- Production deployments only sleep when the app opts in; previews always may.
ALTER TABLE apps ADD COLUMN scale_to_zero_production BOOLEAN NOT NULL DEFAULT false;
