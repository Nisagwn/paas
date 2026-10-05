-- Faz 16: per-app build settings (Vercel's "Build & Development Settings").
--
-- An empty string means "use what the platform detects", so an app without a
-- row builds exactly as before. Values are validated by the API
-- (build.NormalizeSettings) before they are stored.
CREATE TABLE app_build_settings (
    app_id           BIGINT PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    root_directory   TEXT NOT NULL DEFAULT '',  -- monorepo subdirectory, relative
    framework        TEXT NOT NULL DEFAULT '',  -- '' = auto-detect, or a preset id
    install_command  TEXT NOT NULL DEFAULT '',
    build_command    TEXT NOT NULL DEFAULT '',
    start_command    TEXT NOT NULL DEFAULT '',
    output_directory TEXT NOT NULL DEFAULT '',  -- static exports, relative to root_directory
    node_version     TEXT NOT NULL DEFAULT '',  -- '' = the platform default
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The framework the build detected (or was told to use), e.g. "Next.js".
ALTER TABLE deployments ADD COLUMN framework TEXT NOT NULL DEFAULT '';
