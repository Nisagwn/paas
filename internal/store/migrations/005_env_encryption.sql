-- Faz 10: env values are encrypted with AES-256-GCM (internal/secret).
-- app_env.value holds "v1:<key id>:<base64(nonce|ciphertext)>"; key_id
-- names the key that encrypted it. NULL key_id means the value is still
-- plaintext (written while PAAS_ENV_KEY was unset). With a key configured,
-- the control plane encrypts NULL rows and re-encrypts rows under old keys
-- at startup.
ALTER TABLE app_env ADD COLUMN key_id TEXT;
CREATE INDEX app_env_key_id ON app_env (key_id);
