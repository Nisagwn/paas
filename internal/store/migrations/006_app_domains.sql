-- Faz 12: custom domains. A domain is routed to the app's production
-- deployment once DNS proves the owner controls it (CNAME to the app's
-- platform hostname, or a TXT record with the verification token).
--
--   pending  : added, DNS not verified yet (error says what is missing)
--   verified : DNS verified and routed; the certificate is not ready yet
--   active   : routed and, with TLS, its certificate is ready
--   error    : a verified domain failed re-validation; it stays routed
--              (routed = true) until the grace period ends
CREATE TABLE app_domains (
    id                 BIGSERIAL PRIMARY KEY,
    app_id             BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    hostname           TEXT NOT NULL UNIQUE
        CHECK (hostname = lower(hostname) AND length(hostname) BETWEEN 3 AND 253),
    status             TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'verified', 'active', 'error')),
    verification_token TEXT NOT NULL,
    -- How the last successful check passed: cname, txt or skip (dev only).
    verified_by        TEXT NOT NULL DEFAULT '',
    -- Whether an Ingress should exist for the hostname.
    routed             BOOLEAN NOT NULL DEFAULT false,
    error              TEXT NOT NULL DEFAULT '',
    last_checked_at    TIMESTAMPTZ,
    verified_at        TIMESTAMPTZ,
    -- First failed re-validation of a routed domain; starts the grace period.
    failing_since      TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX app_domains_app_idx ON app_domains (app_id);
