-- Faz 26: notifications. Channels belong to a team (optionally narrowed to
-- some of its apps) and subscribe to event types; every event is written to
-- an outbox (one delivery row per matching channel) that a background
-- dispatcher sends with retries.

CREATE TABLE notification_channels (
    id           BIGSERIAL PRIMARY KEY,
    team_id      BIGINT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    name         TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    kind         TEXT NOT NULL CHECK (kind IN ('email', 'slack', 'discord', 'webhook')),
    -- Event types the channel receives (notify.EventTypes).
    events       TEXT[] NOT NULL DEFAULT '{}',
    -- email: explicit addresses, and/or the team members who saved an
    -- address in their preferences.
    recipients   TEXT[] NOT NULL DEFAULT '{}',
    team_members BOOLEAN NOT NULL DEFAULT false,
    -- slack / discord / webhook: {"url": …, "signing_secret": …} sealed with
    -- the env keyring (key_id NULL = plaintext, development mode). The
    -- associated data binds it to the channel id.
    secrets      TEXT NOT NULL DEFAULT '',
    key_id       TEXT,
    -- scheme://host/… of the URL, safe to show.
    url_hint     TEXT NOT NULL DEFAULT '',
    enabled      BOOLEAN NOT NULL DEFAULT true,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (team_id, name)
);

-- No rows: the channel receives events of every app of the team.
CREATE TABLE notification_channel_apps (
    channel_id BIGINT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    app_id     BIGINT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    PRIMARY KEY (channel_id, app_id)
);
CREATE INDEX notification_channel_apps_app_idx ON notification_channel_apps (app_id);

-- An event is stored once per (team, type, key): emitting it again (a
-- retried worker step, a second control plane replica) is a no-op.
CREATE TABLE notification_events (
    id         BIGSERIAL PRIMARY KEY,
    team_id    BIGINT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    app_id     BIGINT REFERENCES apps(id) ON DELETE SET NULL,
    type       TEXT NOT NULL,
    event_key  TEXT NOT NULL,
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (team_id, type, event_key)
);

-- The outbox. idempotency_key is sent to receivers (X-Paas-Delivery,
-- Message-ID), so a delivery repeated after a crash can be recognized.
CREATE TABLE notification_deliveries (
    id              BIGSERIAL PRIMARY KEY,
    event_id        BIGINT NOT NULL REFERENCES notification_events(id) ON DELETE CASCADE,
    channel_id      BIGINT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL UNIQUE,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'skipped')),
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A 'sending' row whose lease ran out belongs to a dead dispatcher and
    -- is claimed again (at-least-once).
    locked_until    TIMESTAMPTZ,
    response_code   INT,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at         TIMESTAMPTZ,
    UNIQUE (event_id, channel_id)
);
CREATE INDEX notification_deliveries_due_idx ON notification_deliveries (next_attempt_at)
    WHERE status IN ('pending', 'sending');
CREATE INDEX notification_deliveries_channel_idx ON notification_deliveries (channel_id, id DESC);
CREATE INDEX notification_deliveries_sent_idx ON notification_deliveries (channel_id, sent_at)
    WHERE status = 'sent';

-- Per-user email preferences: the address team-member email channels use,
-- a global switch and the event types the user opted out of. They also
-- apply when the address is listed explicitly on a channel.
CREATE TABLE notification_preferences (
    user_id       BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    email         TEXT NOT NULL DEFAULT '',
    email_enabled BOOLEAN NOT NULL DEFAULT true,
    muted_events  TEXT[] NOT NULL DEFAULT '{}',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX notification_preferences_email_idx ON notification_preferences (lower(email)) WHERE email <> '';

-- Wakes the dispatchers of every control plane replica.
CREATE FUNCTION notification_outbox_notify() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('notification_outbox', '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER notification_outbox_notify AFTER INSERT ON notification_deliveries
    FOR EACH STATEMENT EXECUTE FUNCTION notification_outbox_notify();
