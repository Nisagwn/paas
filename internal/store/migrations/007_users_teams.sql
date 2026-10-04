-- Faz 13: users, teams, roles and personal API tokens. Apps belong to a team.

-- A user is a GitHub account; github_id is the identity (logins can change).
CREATE TABLE users (
    id         BIGSERIAL PRIMARY KEY,
    github_id  BIGINT NOT NULL UNIQUE,
    login      TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- GitHub logins are case-insensitive and unique at any point in time.
CREATE UNIQUE INDEX users_login_key ON users (lower(login));

CREATE TABLE teams (
    id         BIGSERIAL PRIMARY KEY,
    slug       TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z][a-z0-9-]{1,30}$'),
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE team_members (
    team_id    BIGINT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('owner', 'member', 'viewer')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);
CREATE INDEX team_members_user_idx ON team_members (user_id);

-- Personal API tokens. Only the SHA-256 of the token is stored; prefix is
-- the first characters, shown so users can tell their tokens apart.
CREATE TABLE api_tokens (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    prefix       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ
);
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id);

-- Every existing app moves to the "default" team. Its first owner is set at
-- login (PAAS_ADMIN_GITHUB_LOGINS) or through the API with the admin token.
INSERT INTO teams (slug, name) VALUES ('default', 'Default');

ALTER TABLE apps ADD COLUMN team_id BIGINT REFERENCES teams(id) ON DELETE RESTRICT;
UPDATE apps SET team_id = (SELECT id FROM teams WHERE slug = 'default');
ALTER TABLE apps ALTER COLUMN team_id SET NOT NULL;
CREATE INDEX apps_team_idx ON apps (team_id);
