-- Faz 15: GitHub App. One App replaces the personal token, the OAuth App and
-- the per-repository webhooks: users install it on the repositories they
-- choose, and the platform mints short-lived installation tokens.
--
-- Rows mirror GitHub's installation and installation_repositories webhooks;
-- GitHub is the source of truth and a resync rewrites them.
CREATE TABLE github_installations (
    id            BIGINT PRIMARY KEY,           -- GitHub's installation id
    account_login TEXT NOT NULL,                -- user or organization
    account_type  TEXT NOT NULL CHECK (account_type IN ('User', 'Organization')),
    -- The team that installed it through the platform (setup URL); repos of
    -- an unclaimed installation are not offered for import.
    team_id       BIGINT REFERENCES teams(id) ON DELETE SET NULL,
    suspended     BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE github_installation_repos (
    installation_id BIGINT NOT NULL REFERENCES github_installations(id) ON DELETE CASCADE,
    repo_id         BIGINT NOT NULL,            -- GitHub's repository id (survives renames)
    full_name       TEXT NOT NULL,              -- owner/name as GitHub spells it
    private         BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (installation_id, repo_id)
);

-- GitHub repository names are case-insensitive.
CREATE INDEX github_installation_repos_name ON github_installation_repos (lower(full_name));
