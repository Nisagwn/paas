// Command paas-envkey manages the key that encrypts app env values (Faz 10).
//
//	go run ./cmd/paas-envkey            # print a new random key (PAAS_ENV_KEY)
//	go run ./cmd/paas-envkey generate   # same
//	go run ./cmd/paas-envkey status     # count stored values per key id
//	go run ./cmd/paas-envkey rotate     # encrypt plaintext values and re-encrypt
//	                                    # old-key values under PAAS_ENV_KEY
//
// status and rotate read PAAS_DATABASE_URL, PAAS_ENV_KEY and
// PAAS_ENV_OLD_KEYS. The control plane runs the same rotation at startup;
// this command does it without a restart, e.g. before dropping an old key.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/nisagwn/paas/internal/secret"
	"github.com/nisagwn/paas/internal/store"
)

func main() {
	cmd := "generate"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "generate":
		err = generate()
	case "status":
		err = withStore(false, status)
	case "rotate":
		err = withStore(true, rotate)
	default:
		err = fmt.Errorf("unknown command %q (generate, status, rotate)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "paas-envkey:", err)
		os.Exit(1)
	}
}

func generate() error {
	k, err := secret.GenerateKey()
	if err != nil {
		return err
	}
	parsed, err := secret.ParseKey(k)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "key id: %s\n", parsed.ID)
	fmt.Println(k)
	return nil
}

func withStore(needKey bool, fn func(context.Context, *store.Store, *secret.Keyring) error) error {
	url := os.Getenv("PAAS_DATABASE_URL")
	if url == "" {
		return errors.New("PAAS_DATABASE_URL is not set")
	}
	keys, err := secret.FromEnv()
	if err != nil {
		return err
	}
	if needKey && keys == nil {
		return errors.New("PAAS_ENV_KEY is not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	st.SetEnvKeyring(keys)
	return fn(ctx, st, keys)
}

func status(ctx context.Context, st *store.Store, keys *secret.Keyring) error {
	stats, err := st.EnvKeyStats(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(stats))
	for id := range stats {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		label, note := id, ""
		switch {
		case id == "":
			label = "(plaintext)"
		case keys != nil && id == keys.CurrentID():
			note = " current"
		case keys != nil && keys.Has(id):
			note = " old"
		default:
			note = " UNKNOWN"
		}
		fmt.Printf("%-12s %6d%s\n", label, stats[id], note)
	}
	if len(ids) == 0 {
		fmt.Println("no app env values")
	}
	return nil
}

func rotate(ctx context.Context, st *store.Store, keys *secret.Keyring) error {
	if err := st.CheckEnvKeys(ctx); err != nil {
		return err
	}
	n, err := st.ReencryptEnv(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("re-encrypted %d values under key %s\n", n, keys.CurrentID())
	return nil
}
