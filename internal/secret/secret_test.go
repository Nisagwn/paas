package secret

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func newKey(t *testing.T) string {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func ring(t *testing.T, cur string, old ...string) *Keyring {
	t.Helper()
	k, err := NewKeyring(cur, old...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	k := ring(t, newKey(t))
	aad := []byte("app-1/DATABASE_URL")
	for _, plain := range []string{"", "x", "postgres://u:p@h/db?sslmode=disable", strings.Repeat("ç", 5000)} {
		ct, err := k.Encrypt([]byte(plain), aad)
		if err != nil {
			t.Fatal(err)
		}
		if plain != "" && strings.Contains(ct, plain) {
			t.Fatalf("ciphertext contains plaintext")
		}
		if !strings.HasPrefix(ct, "v1:"+k.CurrentID()+":") {
			t.Fatalf("unexpected prefix: %q", ct)
		}
		got, err := k.Decrypt(ct, aad)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != plain {
			t.Fatalf("got %q, want %q", got, plain)
		}
	}
}

func TestNonceIsRandom(t *testing.T) {
	k := ring(t, newKey(t))
	a, _ := k.Encrypt([]byte("same"), nil)
	b, _ := k.Encrypt([]byte("same"), nil)
	if a == b {
		t.Fatal("two encryptions of the same value are identical")
	}
}

func TestTamperDetection(t *testing.T) {
	k := ring(t, newKey(t))
	ct, _ := k.Encrypt([]byte("secret-value"), []byte("aad"))
	id, payload, err := Parse(ct)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(payload)
	for i := range raw {
		mod := bytes.Clone(raw)
		mod[i] ^= 0x01
		bad := "v1:" + id + ":" + base64.StdEncoding.EncodeToString(mod)
		if _, err := k.Decrypt(bad, []byte("aad")); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("byte %d flipped: err = %v, want ErrDecrypt", i, err)
		}
	}
	// Truncated and malformed inputs.
	for _, bad := range []string{
		"", "plain", "v1:" + id, "v2:" + id + ":" + payload, "v1::" + payload,
		"v1:" + id + ":!!!", "v1:" + id + ":" + base64.StdEncoding.EncodeToString(raw[:10]),
	} {
		if _, err := k.Decrypt(bad, []byte("aad")); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%q: err = %v, want ErrMalformed", bad, err)
		}
	}
}

func TestWrongAAD(t *testing.T) {
	k := ring(t, newKey(t))
	ct, _ := k.Encrypt([]byte("v"), []byte("app-1/KEY"))
	for _, aad := range [][]byte{[]byte("app-2/KEY"), []byte("app-1/OTHER"), nil} {
		if _, err := k.Decrypt(ct, aad); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("aad %q: err = %v, want ErrDecrypt", aad, err)
		}
	}
}

func TestWrongKey(t *testing.T) {
	a := ring(t, "k:"+newKey(t))
	b := ring(t, "k:"+newKey(t)) // same id, different key material
	ct, _ := a.Encrypt([]byte("v"), nil)
	if _, err := b.Decrypt(ct, nil); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("err = %v, want ErrDecrypt", err)
	}
	c := ring(t, newKey(t))
	if _, err := c.Decrypt(ct, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

func TestRotation(t *testing.T) {
	oldKey, newK := newKey(t), newKey(t)
	before := ring(t, oldKey)
	ct, _ := before.Encrypt([]byte("v"), []byte("aad"))

	after := ring(t, newK, oldKey)
	if after.CurrentID() == before.CurrentID() {
		t.Fatal("key ids collide")
	}
	if !after.Has(before.CurrentID()) {
		t.Fatal("old key id not in keyring")
	}
	got, err := after.Decrypt(ct, []byte("aad"))
	if err != nil || string(got) != "v" {
		t.Fatalf("old ciphertext: %q, %v", got, err)
	}
	re, _ := after.Encrypt(got, []byte("aad"))
	if id, _, _ := Parse(re); id != after.CurrentID() {
		t.Fatalf("re-encrypted under %q, want %q", id, after.CurrentID())
	}
	// Once the old key is dropped, old ciphertexts are unreadable.
	if _, err := ring(t, newK).Decrypt(ct, []byte("aad")); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

func TestParseKey(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, 32)
	b64 := base64.StdEncoding.EncodeToString(raw)
	sum := sha256.Sum256(raw)

	k, err := ParseKey(b64)
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(sum[:])[:8]; k.ID != want {
		t.Fatalf("derived id %q, want %q", k.ID, want)
	}
	if k, err := ParseKey("2026-10:" + b64); err != nil || k.ID != "2026-10" {
		t.Fatalf("explicit id: %v, %v", k.ID, err)
	}
	if _, err := ParseKey(base64.RawURLEncoding.EncodeToString(raw)); err != nil {
		t.Fatalf("raw url base64: %v", err)
	}

	for name, bad := range map[string]string{
		"empty":      "",
		"not base64": "not*base64",
		"too short":  base64.StdEncoding.EncodeToString(raw[:16]),
		"too long":   base64.StdEncoding.EncodeToString(append(raw, 1)),
		"bad id":     "bad id:" + b64,
		"empty id":   ":" + b64,
		"long id":    strings.Repeat("a", 33) + ":" + b64,
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewKeyring(t *testing.T) {
	a, b := newKey(t), newKey(t)
	if _, err := NewKeyring(a, a, "", b); err != nil {
		t.Fatalf("duplicate identical key: %v", err)
	}
	if _, err := NewKeyring("x:"+a, "x:"+b); err == nil {
		t.Fatal("two keys with one id accepted")
	}
	if _, err := NewKeyring(a, "garbage"); err == nil {
		t.Fatal("bad old key accepted")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvKey, "")
	t.Setenv(EnvOldKeys, "")
	if k, err := FromEnv(); k != nil || err != nil {
		t.Fatalf("no key: %v, %v", k, err)
	}
	t.Setenv(EnvOldKeys, newKey(t))
	if _, err := FromEnv(); err == nil {
		t.Fatal("old keys without current key accepted")
	}
	cur := newKey(t)
	t.Setenv(EnvKey, cur)
	t.Setenv(EnvOldKeys, newKey(t)+", "+newKey(t))
	k, err := FromEnv()
	if err != nil || k == nil || len(k.keys) != 3 {
		t.Fatalf("keyring: %v, %v", k, err)
	}
	t.Setenv(EnvKey, "short")
	if _, err := FromEnv(); err == nil {
		t.Fatal("bad key accepted")
	}
}
