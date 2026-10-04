// Package domains manages custom domains of apps (Faz 12): hostname
// validation, the DNS records an owner has to create, and the verifier
// that checks them and decides which domains are routed.
package domains

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

// ChallengePrefix is prepended to a hostname for the TXT verification record.
const ChallengePrefix = "_paas-challenge."

var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Normalize lowercases host, drops a trailing dot and validates it as a
// fully qualified domain name outside the platform domain. Internationalized
// names must be given in their ASCII (punycode, "xn--") form.
func Normalize(host, platformDomain string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	platformDomain = strings.TrimSuffix(strings.ToLower(platformDomain), ".")
	switch {
	case h == "":
		return "", errors.New("hostname is required")
	case strings.ContainsAny(h, "/:@ "):
		return "", errors.New("hostname only: no scheme, port or path (e.g. www.example.com)")
	case strings.Contains(h, "*"):
		return "", errors.New("wildcard domains are not supported")
	case len(h) > 253:
		return "", errors.New("hostname is longer than 253 characters")
	}
	for _, r := range h {
		if r > 127 {
			return "", errors.New("use the punycode (xn--) form of internationalized domain names")
		}
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", errors.New("hostname must be a fully qualified domain name (e.g. www.example.com)")
	}
	for _, l := range labels {
		if !labelRe.MatchString(l) {
			return "", errors.New("invalid label " + `"` + l + `"` + ": use 1-63 letters, digits and '-', not at either end")
		}
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", errors.New("IP addresses are not domain names")
	}
	if platformDomain != "" && (h == platformDomain || strings.HasSuffix(h, "."+platformDomain)) {
		return "", errors.New("hostnames under " + platformDomain + " belong to the platform")
	}
	return h, nil
}

// NewToken returns a random verification token for the TXT record.
func NewToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "paas-verify-" + hex.EncodeToString(b)
}

// Record is a DNS record the domain owner creates.
type Record struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
	Note  string `json:"note"`
}

// Records lists the DNS records that verify host for app: a CNAME (which
// also routes traffic), or a TXT record for names that cannot have a CNAME,
// such as a zone apex, together with an A record to the platform address.
func Records(host, app, platformDomain, token string) []Record {
	target := app + "." + platformDomain
	return []Record{
		{Type: "CNAME", Name: host, Value: target,
			Note: "Recommended: routes traffic to the platform and verifies the domain."},
		{Type: "TXT", Name: ChallengePrefix + host, Value: token,
			Note: "Alternative for apex domains: verifies ownership; then point an A (or ALIAS) record at the address of " + target + "."},
	}
}
