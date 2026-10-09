// Package protect implements deployment protection (Faz 24, Vercel's
// "Deployment Protection"): who may open an app's deployment URLs, preview
// aliases and, with scope "all", its production hostnames.
//
// Enforcement runs in Traefik: protected Ingresses reference a ForwardAuth
// Middleware (internal/deploy/protection.go) whose address is this
// package's verify endpoint, so every request to a protected host is first
// asked here (Guard.ServeVerify). A request passes with
//
//   - a valid access cookie on the app host (_paas_protect, signed, bound
//     to the app, the host and the protection version),
//   - the app's bypass secret in the x-paas-protection-bypass header
//     (automation, e.g. end-to-end tests in CI),
//   - a shareable link: ?paas_share=<token> sets the cookie.
//
// Otherwise the browser is sent to the control plane's /_protect/login
// (internal/web): team members sign in there with their platform session,
// password visitors type the password. Both end in a one-time token sent
// back to the app host's /_paas/auth path, which the verify endpoint
// answers by setting the host's cookie.
package protect

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
)

var b64 = base64.RawURLEncoding

// Iterations of PBKDF2-HMAC-SHA256 for new password hashes (OWASP's 2023
// recommendation). The count is stored in each hash, so raising it later
// keeps old hashes valid. Tests lower it.
var Iterations = 600_000

const hashScheme = "pbkdf2-sha256"

// HashPassword returns "pbkdf2-sha256$<iterations>$<salt>$<key>" with a
// random 16-byte salt and a 32-byte key (standard library only).
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, Iterations, 32)
	if err != nil {
		return "", err
	}
	return hashScheme + "$" + strconv.Itoa(Iterations) + "$" + b64.EncodeToString(salt) + "$" + b64.EncodeToString(key), nil
}

// CheckPassword compares password with a HashPassword result in constant
// time. Malformed hashes never match.
func CheckPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	salt, err1 := b64.DecodeString(parts[2])
	want, err2 := b64.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// Token prefixes, so leaked secrets are easy to recognize.
const (
	SharePrefix  = "paas_share_"
	BypassPrefix = "paas_bypass_"
)

// newSecret returns a random secret with prefix, its SHA-256 (hex, the
// stored form) and the prefix shown in listings. Secrets carry 256 random
// bits, so a fast hash suffices.
func newSecret(prefix string) (plain, hash, display string) {
	b := make([]byte, 32)
	rand.Read(b)
	plain = prefix + b64.EncodeToString(b)
	return plain, HashSecret(plain), plain[:len(prefix)+6]
}

// NewShareToken returns a fresh shareable-link token.
func NewShareToken() (plain, hash, display string) { return newSecret(SharePrefix) }

// NewBypassSecret returns a fresh protection bypass secret.
func NewBypassSecret() (plain, hash, display string) { return newSecret(BypassPrefix) }

// HashSecret is the stored form of a share token or bypass secret.
func HashSecret(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// secretMatches compares a presented secret with a stored hash.
func secretMatches(plain, hash string) bool {
	return plain != "" && hash != "" && hmac.Equal([]byte(HashSecret(plain)), []byte(hash))
}
