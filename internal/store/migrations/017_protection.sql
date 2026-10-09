-- Faz 24: deployment protection (Vercel's "Deployment Protection").
--
-- An app without a row is unprotected (mode off, scope previews).
--   mode   off      : no protection
--          team     : signed-in members of the app's team (any role)
--          password : a shared password; team members pass as well
--   scope  previews : deployment URLs and preview aliases; production and
--                     custom domains stay public
--          all      : every hostname of the app
-- password_hash is PBKDF2-HMAC-SHA256 ("pbkdf2-sha256$<iter>$<salt>$<key>").
-- version grows whenever the mode or the password changes; access cookies
-- carry it, so such a change signs every visitor out.
-- The bypass secret (x-paas-protection-bypass header, for automation) is
-- stored as its SHA-256 only, like deploy hooks and API tokens.
CREATE TABLE app_protection (
    app_id            BIGINT PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    mode              TEXT NOT NULL DEFAULT 'off' CHECK (mode IN ('off', 'team', 'password')),
    scope             TEXT NOT NULL DEFAULT 'previews' CHECK (scope IN ('previews', 'all')),
    password_hash     TEXT NOT NULL DEFAULT '',
    bypass_hash       TEXT NOT NULL DEFAULT '' CHECK (bypass_hash = '' OR bypass_hash ~ '^[0-9a-f]{64}$'),
    bypass_prefix     TEXT NOT NULL DEFAULT '',
    bypass_created_at TIMESTAMPTZ,
    version           BIGINT NOT NULL DEFAULT 1,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (mode <> 'password' OR password_hash <> '')
);

-- Shareable links: ?paas_share=<token> on any protected URL of the app
-- grants access (until expires_at, or until the link is revoked).
CREATE TABLE protection_share_links (
    id           BIGSERIAL PRIMARY KEY,
    app_id       BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name         TEXT NOT NULL CHECK (length(name) <= 100),
    token_hash   TEXT NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    prefix       TEXT NOT NULL,
    created_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ
);
CREATE INDEX protection_share_links_app_idx ON protection_share_links (app_id);

-- One-time login tokens (control plane → app host) that were redeemed.
-- Rows outlive the token's short lifetime only until the next insert.
CREATE TABLE protection_nonces (
    nonce      TEXT PRIMARY KEY,
    expires_at TIMESTAMPTZ NOT NULL
);
