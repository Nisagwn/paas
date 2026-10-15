-- Faz 30: image retention follows deployment retention.
--
-- image_released_at is set when the cleanup decided that a finished
-- (retired, failed or canceled) deployment's image is no longer referenced
-- by any deployment the platform keeps and deletes it from the registry
-- (store.ReleaseImage). A redeploy that would reuse the image of such a
-- deployment builds it again instead (store.CopyDeployment), and a revived
-- deployment starts without an image, so the column is cleared then.
ALTER TABLE deployments ADD COLUMN image_released_at TIMESTAMPTZ;
