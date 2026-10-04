package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nisagwn/paas/internal/secret"
	"github.com/nisagwn/paas/internal/store"
)

// setupEnvEncryption (Faz 10) loads PAAS_ENV_KEY / PAAS_ENV_OLD_KEYS into
// the store. With a key, plaintext values and values under old keys are
// rewritten under the current key before anything is served. Without a key
// the process refuses to start if encrypted values exist, and otherwise
// keeps storing plaintext with a warning (development mode).
func setupEnvEncryption(ctx context.Context, st *store.Store, log *slog.Logger) error {
	keys, err := secret.FromEnv()
	if err != nil {
		return err
	}
	st.SetEnvKeyring(keys)
	if err := st.CheckEnvKeys(ctx); err != nil {
		return fmt.Errorf("env encryption: %w", err)
	}
	if keys == nil {
		log.Warn("PAAS_ENV_KEY is not set: app env values are stored in plaintext; " +
			"generate a key with `go run ./cmd/paas-envkey` for any shared or production setup")
		return nil
	}
	n, err := st.ReencryptEnv(ctx)
	if err != nil {
		return fmt.Errorf("env encryption: %w", err)
	}
	log.Info("app env encryption enabled", "key_id", keys.CurrentID(), "reencrypted", n)
	return nil
}
