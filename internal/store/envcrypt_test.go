package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/secret"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func keyring(t *testing.T, cur string, old ...string) *secret.Keyring {
	t.Helper()
	k, err := secret.NewKeyring(cur, old...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func genKey(t *testing.T) string {
	t.Helper()
	k, err := secret.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", os.Getenv("PAAS_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

type rawRow struct{ value, keyID string }

// rawEnv reads app_env as stored, bypassing the store.
func rawEnv(t *testing.T, db *sql.DB, appID int64) map[string]rawRow {
	t.Helper()
	rows, err := db.Query(`SELECT key, value, COALESCE(key_id, '') FROM app_env WHERE app_id = $1`, appID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]rawRow{}
	for rows.Next() {
		var k string
		var r rawRow
		if err := rows.Scan(&k, &r.value, &r.keyID); err != nil {
			t.Fatal(err)
		}
		out[k] = r
	}
	return out
}

func setEnv(t *testing.T, st *store.Store, appID int64, kv map[string]string) {
	t.Helper()
	changes := map[string]*string{}
	for k, v := range kv {
		changes[k] = &v
	}
	if err := st.UpdateAppEnv(context.Background(), appID, changes); err != nil {
		t.Fatal(err)
	}
}

func checkEnv(t *testing.T, st *store.Store, appID int64, want map[string]string) {
	t.Helper()
	got, err := st.AppEnv(context.Background(), appID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

var sampleEnv = map[string]string{
	"DATABASE_URL": "postgres://user:hunter2@db/app",
	"API_SECRET":   "s3cr3t-value-123",
	"EMPTY":        "",
}

func TestEnvEncryptedAtRest(t *testing.T) {
	st := testdb.Open(t)
	db := rawDB(t)
	ctx := context.Background()
	keys := keyring(t, genKey(t))
	st.SetEnvKeyring(keys)
	app, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}

	setEnv(t, st, app.ID, sampleEnv)
	checkEnv(t, st, app.ID, sampleEnv)

	for k, r := range rawEnv(t, db, app.ID) {
		if r.keyID != keys.CurrentID() {
			t.Fatalf("%s: key_id %q, want %q", k, r.keyID, keys.CurrentID())
		}
		if !strings.HasPrefix(r.value, "v1:"+keys.CurrentID()+":") {
			t.Fatalf("%s: stored value %q is not ciphertext", k, r.value)
		}
		if v := sampleEnv[k]; v != "" && strings.Contains(r.value, v) {
			t.Fatalf("%s: plaintext visible in table", k)
		}
	}
	// Deletion still works on encrypted rows.
	if err := st.UpdateAppEnv(ctx, app.ID, map[string]*string{"EMPTY": nil}); err != nil {
		t.Fatal(err)
	}
	checkEnv(t, st, app.ID, map[string]string{"DATABASE_URL": sampleEnv["DATABASE_URL"], "API_SECRET": sampleEnv["API_SECRET"]})
}

// A ciphertext moved to another app or variable must not decrypt.
func TestEnvCiphertextBoundToAppAndKey(t *testing.T) {
	st := testdb.Open(t)
	db := rawDB(t)
	ctx := context.Background()
	st.SetEnvKeyring(keyring(t, genKey(t)))
	a := mustApp(t, st, "alpha")
	b := mustApp(t, st, "beta")
	setEnv(t, st, a.ID, map[string]string{"TOKEN": "a-token", "OTHER": "x"})
	setEnv(t, st, b.ID, map[string]string{"TOKEN": "b-token"})

	if _, err := db.Exec(`UPDATE app_env SET value = (SELECT value FROM app_env WHERE app_id = $1 AND key = 'TOKEN')
		WHERE app_id = $2 AND key = 'TOKEN'`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppEnv(ctx, b.ID); !errors.Is(err, secret.ErrDecrypt) {
		t.Fatalf("swapped app: err = %v, want ErrDecrypt", err)
	}
	if _, err := db.Exec(`UPDATE app_env SET value = (SELECT value FROM app_env WHERE app_id = $1 AND key = 'TOKEN')
		WHERE app_id = $1 AND key = 'OTHER'`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppEnv(ctx, a.ID); !errors.Is(err, secret.ErrDecrypt) {
		t.Fatalf("swapped key: err = %v, want ErrDecrypt", err)
	}
}

// Plaintext rows written without a key are encrypted once a key is set;
// rotation moves every row to the new key; both runs are idempotent.
func TestEnvEncryptExistingAndRotate(t *testing.T) {
	st := testdb.Open(t)
	db := rawDB(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")

	// Development mode: no key, plaintext.
	setEnv(t, st, app.ID, sampleEnv)
	for k, r := range rawEnv(t, db, app.ID) {
		if r.keyID != "" || r.value != sampleEnv[k] {
			t.Fatalf("%s: stored %+v without a key, want plaintext", k, r)
		}
	}
	if err := st.CheckEnvKeys(ctx); err != nil {
		t.Fatalf("plaintext rows without a key: %v", err)
	}
	if _, err := st.ReencryptEnv(ctx); err == nil {
		t.Fatal("ReencryptEnv without a key succeeded")
	}

	// Key 1: existing plaintext gets encrypted.
	k1 := genKey(t)
	ring1 := keyring(t, k1)
	st.SetEnvKeyring(ring1)
	if n, err := st.ReencryptEnv(ctx); err != nil || n != len(sampleEnv) {
		t.Fatalf("encrypt existing: n=%d err=%v", n, err)
	}
	if n, err := st.ReencryptEnv(ctx); err != nil || n != 0 {
		t.Fatalf("second run: n=%d err=%v, want 0", n, err)
	}
	before := rawEnv(t, db, app.ID)
	for k, r := range before {
		if r.keyID != ring1.CurrentID() || r.value == sampleEnv[k] {
			t.Fatalf("%s: not encrypted under key 1: %+v", k, r)
		}
	}
	checkEnv(t, st, app.ID, sampleEnv)

	// Key 2 with key 1 as old key: everything moves to key 2.
	k2 := genKey(t)
	ring2 := keyring(t, k2, k1)
	st.SetEnvKeyring(ring2)
	checkEnv(t, st, app.ID, sampleEnv) // readable before rotation
	if n, err := st.ReencryptEnv(ctx); err != nil || n != len(sampleEnv) {
		t.Fatalf("rotate: n=%d err=%v", n, err)
	}
	for k, r := range rawEnv(t, db, app.ID) {
		if r.keyID != ring2.CurrentID() || r.value == before[k].value {
			t.Fatalf("%s: not re-encrypted under key 2: %+v", k, r)
		}
	}
	if n, _ := st.ReencryptEnv(ctx); n != 0 {
		t.Fatalf("second rotate rewrote %d rows", n)
	}

	// The old key can now be dropped.
	st.SetEnvKeyring(keyring(t, k2))
	if err := st.CheckEnvKeys(ctx); err != nil {
		t.Fatal(err)
	}
	checkEnv(t, st, app.ID, sampleEnv)
}

func TestEnvMissingOrUnknownKey(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app := mustApp(t, st, "blog")
	k1 := genKey(t)
	st.SetEnvKeyring(keyring(t, k1))
	setEnv(t, st, app.ID, sampleEnv)

	// No key: encrypted rows exist, so startup must refuse and reads fail.
	st.SetEnvKeyring(nil)
	if err := st.CheckEnvKeys(ctx); !errors.Is(err, store.ErrEnvKeyMissing) {
		t.Fatalf("CheckEnvKeys without key: %v", err)
	}
	if _, err := st.AppEnv(ctx, app.ID); !errors.Is(err, store.ErrEnvKeyMissing) {
		t.Fatalf("AppEnv without key: %v", err)
	}

	// A different key without the old one: unknown key id.
	st.SetEnvKeyring(keyring(t, genKey(t)))
	if err := st.CheckEnvKeys(ctx); err == nil {
		t.Fatal("CheckEnvKeys accepted an unknown key id")
	}
	if _, err := st.AppEnv(ctx, app.ID); !errors.Is(err, secret.ErrUnknownKey) {
		t.Fatalf("AppEnv with unknown key id: %v", err)
	}
	if _, err := st.ReencryptEnv(ctx); err == nil {
		t.Fatal("ReencryptEnv succeeded with an unknown key id")
	}

	// The failed rotation left the rows untouched.
	st.SetEnvKeyring(keyring(t, k1))
	checkEnv(t, st, app.ID, sampleEnv)
}

func mustApp(t *testing.T, st *store.Store, name string) store.App {
	t.Helper()
	app, err := st.CreateApp(context.Background(), name, "nisagwn/"+name, "main")
	if err != nil {
		t.Fatal(err)
	}
	return app
}
