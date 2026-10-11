package notify

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/mail"
	"net/url"
	"strings"

	"github.com/nisagwn/paas/internal/store"
)

// Channel settings as the API and the web UI submit them, and their
// validation (shared, so both refuse the same input).

// MaxRecipients bounds the explicit addresses of an email channel.
const MaxRecipients = 20

// MaxChannels bounds the channels of a team.
const MaxChannels = 50

// ChannelRequest creates or updates a channel. On an update, nil fields
// (and an empty URL) keep the stored value.
type ChannelRequest struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	URL         string    `json:"url,omitempty"`
	Events      *[]string `json:"events,omitempty"`
	Recipients  *[]string `json:"recipients,omitempty"`
	TeamMembers *bool     `json:"team_members,omitempty"`
	// Apps narrows the channel to these apps of the team; [] is every app.
	Apps    *[]string `json:"apps,omitempty"`
	Enabled *bool     `json:"enabled,omitempty"`
	// RotateSecret issues a new signing secret (webhook).
	RotateSecret bool `json:"rotate_secret,omitempty"`
}

// InvalidError is rejected input; its (English) message is safe to show.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(msg string) error { return &InvalidError{msg} }

// IsInvalid returns the message of an *InvalidError.
func IsInvalid(err error) (string, bool) {
	var ie *InvalidError
	if errors.As(err, &ie) {
		return ie.Msg, true
	}
	return "", false
}

// Validation options of a Service.
type Validation struct {
	// AllowInsecureURLs accepts http:// webhook URLs (development).
	AllowInsecureURLs bool
}

// NewSigningSecret returns a random webhook signing secret.
func NewSigningSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b)
}

// URLHint is the part of a secret URL that is safe to show.
func URLHint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/…"
}

func (s *Service) checkURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return invalid("url is required for slack, discord and webhook channels")
	}
	if len(raw) > 2048 {
		return invalid("url is longer than 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return invalid("url must be an absolute https:// URL")
	}
	if u.Scheme == "http" && !s.Validation.AllowInsecureURLs {
		return invalid("url must be an absolute https:// URL")
	}
	if u.User != nil {
		return invalid("url must not contain a user name or password")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !PublicIP(ip) && !s.Validation.AllowInsecureURLs {
		return invalid("url must not point at a private or local address")
	}
	return nil
}

// ChannelInput validates req for team teamID. existing is the stored
// channel on an update (nil on create). secret is a new webhook signing
// secret, to be shown once ("" when unchanged).
func (s *Service) ChannelInput(ctx context.Context, teamID int64, req ChannelRequest, existing *store.NotificationChannel) (in store.ChannelInput, secret string, err error) {
	in.Name = strings.TrimSpace(req.Name)
	if in.Name == "" && existing != nil {
		in.Name = existing.Name
	}
	if in.Name == "" || len([]rune(in.Name)) > 100 || strings.ContainsAny(in.Name, "\r\n") {
		return in, "", invalid("name must be 1-100 characters on one line")
	}
	in.Kind = strings.TrimSpace(req.Kind)
	switch {
	case existing != nil && in.Kind == "":
		in.Kind = existing.Kind
	case existing != nil && in.Kind != existing.Kind:
		return in, "", invalid("the kind of a channel cannot change; create a new channel")
	case !store.ValidChannelKind(in.Kind):
		return in, "", invalid("kind must be email, slack, discord or webhook")
	}

	// Events.
	switch {
	case req.Events != nil:
		seen := map[string]bool{}
		for _, e := range *req.Events {
			e = strings.TrimSpace(e)
			if e == "" || seen[e] {
				continue
			}
			if !ValidEventType(e) {
				return in, "", invalid("unknown event type " + e + "; use one of: " + strings.Join(EventTypes, ", "))
			}
			seen[e] = true
		}
		for _, e := range EventTypes { // stored in display order
			if seen[e] {
				in.Events = append(in.Events, e)
			}
		}
		if len(in.Events) == 0 {
			return in, "", invalid("choose at least one event")
		}
	case existing != nil:
		in.Events = existing.Events
	default:
		in.Events = append([]string{}, DefaultEvents...)
	}

	in.Enabled = true
	if req.Enabled != nil {
		in.Enabled = *req.Enabled
	} else if existing != nil {
		in.Enabled = existing.Enabled
	}

	// Apps.
	switch {
	case req.Apps != nil:
		for _, name := range *req.Apps {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			app, err := s.Store.GetAppByName(ctx, name)
			if errors.Is(err, store.ErrNotFound) || (err == nil && app.TeamID != teamID) {
				return in, "", invalid("app " + name + " is not an app of this team")
			}
			if err != nil {
				return in, "", err
			}
			in.AppIDs = append(in.AppIDs, app.ID)
		}
	case existing != nil:
		in.AppIDs = existing.AppIDs
	}

	switch in.Kind {
	case store.ChannelEmail:
		if !s.SMTPConfigured {
			return in, "", invalid("email channels need an SMTP server: set PAAS_SMTP_HOST and PAAS_SMTP_FROM")
		}
		if req.URL != "" {
			return in, "", invalid("email channels have no url; use recipients")
		}
		if req.Recipients != nil {
			seen := map[string]bool{}
			for _, r := range *req.Recipients {
				r = strings.TrimSpace(r)
				if r == "" {
					continue
				}
				a, err := mail.ParseAddress(r)
				if err != nil || a.Name != "" || !strings.Contains(a.Address, "@") {
					return in, "", invalid("invalid email address " + r)
				}
				if k := strings.ToLower(a.Address); !seen[k] {
					seen[k] = true
					in.Recipients = append(in.Recipients, a.Address)
				}
			}
		} else if existing != nil {
			in.Recipients = existing.Recipients
		}
		if len(in.Recipients) > MaxRecipients {
			return in, "", invalid("an email channel can have at most 20 addresses")
		}
		if req.TeamMembers != nil {
			in.TeamMembers = *req.TeamMembers
		} else if existing != nil {
			in.TeamMembers = existing.TeamMembers
		}
		if len(in.Recipients) == 0 && !in.TeamMembers {
			return in, "", invalid("add at least one address or send to the team members")
		}
	default:
		if req.Recipients != nil && len(*req.Recipients) > 0 || req.TeamMembers != nil && *req.TeamMembers {
			return in, "", invalid("recipients only apply to email channels")
		}
		url := strings.TrimSpace(req.URL)
		if url != "" || existing == nil {
			if err := s.checkURL(url); err != nil {
				return in, "", err
			}
		}
		if existing != nil && url == "" && !req.RotateSecret {
			break // secrets unchanged
		}
		sec := store.ChannelSecrets{URL: url}
		if existing != nil {
			old, err := s.Store.NotificationChannelSecrets(ctx, existing.ID)
			if err != nil {
				return in, "", err
			}
			if sec.URL == "" {
				sec.URL = old.URL
			}
			sec.SigningSecret = old.SigningSecret
		}
		if in.Kind == store.ChannelWebhook && (existing == nil || req.RotateSecret || sec.SigningSecret == "") {
			sec.SigningSecret = NewSigningSecret()
			secret = sec.SigningSecret
		} else if req.RotateSecret {
			return in, "", invalid("only webhook channels have a signing secret")
		}
		in.Secrets = &sec
		in.URLHint = URLHint(sec.URL)
	}
	return in, secret, nil
}

// CreateChannel validates and stores a new channel; secret is the webhook
// signing secret, shown once.
func (s *Service) CreateChannel(ctx context.Context, teamID int64, req ChannelRequest, by string) (store.NotificationChannel, string, error) {
	existing, err := s.Store.ListNotificationChannels(ctx, teamID)
	if err != nil {
		return store.NotificationChannel{}, "", err
	}
	if len(existing) >= MaxChannels {
		return store.NotificationChannel{}, "", invalid("a team can have at most 50 notification channels")
	}
	in, secret, err := s.ChannelInput(ctx, teamID, req, nil)
	if err != nil {
		return store.NotificationChannel{}, "", err
	}
	in.CreatedBy = by
	ch, err := s.Store.CreateNotificationChannel(ctx, teamID, in)
	if errors.Is(err, store.ErrConflict) {
		return ch, "", invalid("a channel with this name already exists")
	}
	return ch, secret, err
}

// UpdateChannel validates and stores new settings of channel ch.
func (s *Service) UpdateChannel(ctx context.Context, ch store.NotificationChannel, req ChannelRequest) (store.NotificationChannel, string, error) {
	in, secret, err := s.ChannelInput(ctx, ch.TeamID, req, &ch)
	if err != nil {
		return ch, "", err
	}
	out, err := s.Store.UpdateNotificationChannel(ctx, ch.TeamID, ch.ID, in)
	if errors.Is(err, store.ErrConflict) {
		return ch, "", invalid("a channel with this name already exists")
	}
	return out, secret, err
}

// CheckPrefs validates a user's email preferences.
func CheckPrefs(p *store.NotificationPrefs) error {
	p.Email = strings.TrimSpace(p.Email)
	if p.Email != "" {
		a, err := mail.ParseAddress(p.Email)
		if err != nil || a.Name != "" {
			return invalid("invalid email address " + p.Email)
		}
		p.Email = a.Address
	}
	seen := map[string]bool{}
	var muted []string
	for _, e := range p.MutedEvents {
		if !ValidEventType(e) {
			return invalid("unknown event type " + e + "; use one of: " + strings.Join(EventTypes, ", "))
		}
		if !seen[e] {
			seen[e] = true
			muted = append(muted, e)
		}
	}
	p.MutedEvents = muted
	return nil
}
