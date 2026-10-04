package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/nisagwn/paas/internal/secret"
)

// Faz 10: encryption of app_env values (migration 005).
//
// With a keyring every written value is AES-256-GCM ciphertext and key_id
// names its key; without one values are stored as plaintext with a NULL
// key_id. The associated data binds each ciphertext to its app and variable
// name, so rows cannot be swapped between apps or keys.

// ErrEnvKeyMissing is returned when encrypted env values exist but the
// store has no keyring to read them.
var ErrEnvKeyMissing = errors.New("app env values are encrypted but PAAS_ENV_KEY is not set")

// SetEnvKeyring makes the store encrypt env values it writes and decrypt
// the ones it reads. nil keeps plaintext. Call it before serving requests.
func (s *Store) SetEnvKeyring(k *secret.Keyring) { s.env = k }

func envAAD(appID int64, key string) []byte {
	return []byte("paas/app_env\x00" + strconv.FormatInt(appID, 10) + "\x00" + key)
}

// sealEnv returns the stored form of a value and its key id (NULL = plaintext).
func (s *Store) sealEnv(appID int64, key, value string) (string, sql.NullString, error) {
	if s.env == nil {
		return value, sql.NullString{}, nil
	}
	ct, err := s.env.Encrypt([]byte(value), envAAD(appID, key))
	if err != nil {
		return "", sql.NullString{}, err
	}
	return ct, sql.NullString{String: s.env.CurrentID(), Valid: true}, nil
}

// openEnv reverses sealEnv.
func (s *Store) openEnv(appID int64, key, stored string, keyID sql.NullString) (string, error) {
	if !keyID.Valid {
		return stored, nil
	}
	if s.env == nil {
		return "", ErrEnvKeyMissing
	}
	plain, err := s.env.Decrypt(stored, envAAD(appID, key))
	if err != nil {
		return "", fmt.Errorf("app %d env %s: %w", appID, key, err)
	}
	return string(plain), nil
}

// EnvKeyStats counts env rows per key id; "" counts plaintext rows.
func (s *Store) EnvKeyStats(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(key_id, ''), count(*) FROM app_env GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// CheckEnvKeys verifies that every stored env value can be read with the
// configured keyring: without a keyring there must be no encrypted rows,
// with one every key id must be known.
func (s *Store) CheckEnvKeys(ctx context.Context) error {
	stats, err := s.EnvKeyStats(ctx)
	if err != nil {
		return err
	}
	for id, n := range stats {
		if id == "" {
			continue
		}
		if s.env == nil {
			return fmt.Errorf("%w (%d values under key %q)", ErrEnvKeyMissing, n, id)
		}
		if !s.env.Has(id) {
			return fmt.Errorf("%d app env values use key %q, which is neither PAAS_ENV_KEY nor in PAAS_ENV_OLD_KEYS", n, id)
		}
	}
	return nil
}

// ReencryptEnv encrypts plaintext env values and re-encrypts values under
// old keys with the current key, all in one transaction. It is idempotent:
// rows already under the current key are not touched. It returns the
// number of rows rewritten.
func (s *Store) ReencryptEnv(ctx context.Context) (int, error) {
	if s.env == nil {
		return 0, errors.New("re-encrypting app env values needs PAAS_ENV_KEY")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	type row struct {
		appID      int64
		key, value string
		keyID      sql.NullString
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT app_id, key, value, key_id FROM app_env
		WHERE key_id IS DISTINCT FROM $1
		ORDER BY app_id, key FOR UPDATE`, s.env.CurrentID())
	if err != nil {
		return 0, err
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.appID, &r.key, &r.value, &r.keyID); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, r := range todo {
		plain, err := s.openEnv(r.appID, r.key, r.value, r.keyID)
		if err != nil {
			return 0, err
		}
		value, keyID, err := s.sealEnv(r.appID, r.key, plain)
		if err != nil {
			return 0, err
		}
		// updated_at is left alone: the value itself did not change.
		if _, err := tx.ExecContext(ctx,
			`UPDATE app_env SET value = $3, key_id = $4 WHERE app_id = $1 AND key = $2`,
			r.appID, r.key, value, keyID); err != nil {
			return 0, err
		}
	}
	return len(todo), tx.Commit()
}
