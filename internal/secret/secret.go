// Package secret encrypts short values (app environment variables) with
// AES-256-GCM under a keyring that supports key rotation.
//
// Ciphertext format:
//
//	v1:<key id>:<base64(nonce | ciphertext+tag)>
//
// The nonce is 12 random bytes per value. Callers pass associated data
// (e.g. app id + variable name) that is authenticated but not stored, so a
// ciphertext copied to another app or variable fails to decrypt.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	// KeySize is the AES-256 key length in bytes.
	KeySize = 32
	version = "v1"
	// EnvKey holds the current key; EnvOldKeys holds comma-separated keys
	// that are only used to decrypt values written before a rotation.
	EnvKey     = "PAAS_ENV_KEY"
	EnvOldKeys = "PAAS_ENV_OLD_KEYS"
)

var (
	ErrMalformed  = errors.New("secret: malformed ciphertext")
	ErrUnknownKey = errors.New("secret: ciphertext uses an unknown key id")
	ErrDecrypt    = errors.New("secret: decryption failed (wrong key, wrong context or tampered value)")
)

var keyIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// Key is one AES-256 key with its identifier.
type Key struct {
	ID   string
	aead cipher.AEAD
}

// ParseKey accepts "base64" (id = first 8 hex chars of sha256(key)) or
// "id:base64". The decoded key must be exactly 32 bytes.
func ParseKey(s string) (Key, error) {
	s = strings.TrimSpace(s)
	id, b64 := "", s
	if i := strings.IndexByte(s, ':'); i >= 0 {
		id, b64 = s[:i], s[i+1:]
		if !keyIDRe.MatchString(id) {
			return Key{}, fmt.Errorf("secret: invalid key id %q (letters, digits, '_', '-', '.'; max 32)", id)
		}
	}
	raw, err := decodeBase64(b64)
	if err != nil {
		return Key{}, errors.New("secret: key is not valid base64")
	}
	if len(raw) != KeySize {
		return Key{}, fmt.Errorf("secret: key must be %d bytes, got %d", KeySize, len(raw))
	}
	if id == "" {
		sum := sha256.Sum256(raw)
		id = hex.EncodeToString(sum[:])[:8]
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return Key{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Key{}, err
	}
	return Key{ID: id, aead: aead}, nil
}

func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

// GenerateKey returns a new random key, base64 encoded (PAAS_ENV_KEY format).
func GenerateKey() (string, error) {
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Keyring encrypts with the current key and decrypts with any known key.
type Keyring struct {
	current Key
	keys    map[string]Key
}

// NewKeyring builds a keyring from the current key and older keys.
func NewKeyring(current string, old ...string) (*Keyring, error) {
	cur, err := ParseKey(current)
	if err != nil {
		return nil, fmt.Errorf("current key: %w", err)
	}
	k := &Keyring{current: cur, keys: map[string]Key{cur.ID: cur}}
	for i, s := range old {
		if strings.TrimSpace(s) == "" {
			continue
		}
		o, err := ParseKey(s)
		if err != nil {
			return nil, fmt.Errorf("old key %d: %w", i+1, err)
		}
		if prev, dup := k.keys[o.ID]; dup {
			// The same key listed twice is harmless; two keys with one id are not.
			if !sameKey(prev, o) {
				return nil, fmt.Errorf("old key %d: key id %q is already in use", i+1, o.ID)
			}
			continue
		}
		k.keys[o.ID] = o
	}
	return k, nil
}

// sameKey compares two keys by sealing a fixed input with a fixed nonce.
func sameKey(a, b Key) bool {
	nonce := make([]byte, a.aead.NonceSize())
	return string(a.aead.Seal(nil, nonce, nil, nil)) == string(b.aead.Seal(nil, nonce, nil, nil))
}

// FromEnv reads PAAS_ENV_KEY and PAAS_ENV_OLD_KEYS. It returns (nil, nil)
// when no key is configured; old keys without a current key are an error.
func FromEnv() (*Keyring, error) {
	cur := strings.TrimSpace(os.Getenv(EnvKey))
	old := strings.TrimSpace(os.Getenv(EnvOldKeys))
	if cur == "" {
		if old != "" {
			return nil, fmt.Errorf("%s is set but %s is not", EnvOldKeys, EnvKey)
		}
		return nil, nil
	}
	k, err := NewKeyring(cur, strings.Split(old, ",")...)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", EnvKey, EnvOldKeys, err)
	}
	return k, nil
}

// CurrentID is the id of the key new values are encrypted with.
func (k *Keyring) CurrentID() string { return k.current.ID }

// Has reports whether the keyring can decrypt values of the given key id.
func (k *Keyring) Has(id string) bool { _, ok := k.keys[id]; return ok }

// Encrypt seals plaintext with the current key, binding it to aad.
func (k *Keyring) Encrypt(plaintext, aad []byte) (string, error) {
	nonce := make([]byte, k.current.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := k.current.aead.Seal(nonce, nonce, plaintext, aad)
	return version + ":" + k.current.ID + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a ciphertext produced by Encrypt with the same aad.
func (k *Keyring) Decrypt(ciphertext string, aad []byte) ([]byte, error) {
	id, payload, err := Parse(ciphertext)
	if err != nil {
		return nil, err
	}
	key, ok := k.keys[id]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownKey, id)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	ns := key.aead.NonceSize()
	if err != nil || len(raw) < ns+key.aead.Overhead() {
		return nil, ErrMalformed
	}
	plain, err := key.aead.Open(nil, raw[:ns], raw[ns:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

// Parse splits a ciphertext into its key id and base64 payload.
func Parse(ciphertext string) (keyID, payload string, err error) {
	parts := strings.SplitN(ciphertext, ":", 3)
	if len(parts) != 3 || parts[0] != version || !keyIDRe.MatchString(parts[1]) || parts[2] == "" {
		return "", "", ErrMalformed
	}
	return parts[1], parts[2], nil
}
