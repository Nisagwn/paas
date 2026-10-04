package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Custom domain statuses (migration 006).
const (
	DomainPending  = "pending"
	DomainVerified = "verified"
	DomainActive   = "active"
	DomainError    = "error"
)

// AliasCustom is the kind of AliasRoute that AliasRoutes returns for a
// routed custom domain. It always targets the production deployment.
const AliasCustom = "custom"

// MaxDomainsPerApp bounds custom domains per app (one Ingress and one
// certificate each).
const MaxDomainsPerApp = 20

var ErrTooManyDomains = errors.New("too many domains for this app")

// Domain is a custom hostname of an app.
type Domain struct {
	ID            int64      `json:"-"`
	AppID         int64      `json:"-"`
	AppName       string     `json:"app"`
	Hostname      string     `json:"hostname"`
	Status        string     `json:"status"`
	Token         string     `json:"verification_token"`
	VerifiedBy    string     `json:"verified_by,omitempty"`
	Routed        bool       `json:"routed"`
	Error         string     `json:"error,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
	FailingSince  *time.Time `json:"failing_since,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

const domainCols = `dm.id, dm.app_id, a.name, dm.hostname, dm.status, dm.verification_token, dm.verified_by,
	dm.routed, dm.error, dm.last_checked_at, dm.verified_at, dm.failing_since, dm.created_at`

func scanDomain(row interface{ Scan(...any) error }) (Domain, error) {
	var d Domain
	err := row.Scan(&d.ID, &d.AppID, &d.AppName, &d.Hostname, &d.Status, &d.Token, &d.VerifiedBy,
		&d.Routed, &d.Error, &d.LastCheckedAt, &d.VerifiedAt, &d.FailingSince, &d.CreatedAt)
	return d, err
}

// AddDomain registers a pending domain. The hostname must already be
// validated and lowercase. A hostname is unique across all apps.
func (s *Store) AddDomain(ctx context.Context, appID int64, hostname, token string) (Domain, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Domain{}, err
	}
	defer tx.Rollback()
	// Lock the app row so concurrent adds cannot both pass the limit.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM apps WHERE id = $1 FOR UPDATE`, appID); err != nil {
		return Domain{}, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM app_domains WHERE app_id = $1`, appID).Scan(&n); err != nil {
		return Domain{}, err
	}
	if n >= MaxDomainsPerApp {
		return Domain{}, ErrTooManyDomains
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO app_domains (app_id, hostname, verification_token) VALUES ($1, $2, $3) RETURNING id`,
		appID, hostname, token).Scan(&id)
	if isUniqueViolation(err) {
		return Domain{}, ErrConflict
	}
	if err != nil {
		return Domain{}, err
	}
	if err := tx.Commit(); err != nil {
		return Domain{}, err
	}
	return s.getDomain(ctx, `dm.id = $1`, id)
}

// GetDomain returns one domain of an app.
func (s *Store) GetDomain(ctx context.Context, appID int64, hostname string) (Domain, error) {
	return s.getDomain(ctx, `dm.app_id = $1 AND dm.hostname = $2`, appID, hostname)
}

func (s *Store) getDomain(ctx context.Context, where string, args ...any) (Domain, error) {
	d, err := scanDomain(s.db.QueryRowContext(ctx, `
		SELECT `+domainCols+` FROM app_domains dm JOIN apps a ON a.id = dm.app_id WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// ListDomains returns the app's domains; appID 0 returns every app's.
func (s *Store) ListDomains(ctx context.Context, appID int64) ([]Domain, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+domainCols+` FROM app_domains dm JOIN apps a ON a.id = dm.app_id
		WHERE $1 = 0 OR dm.app_id = $1 ORDER BY dm.hostname`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Domain{}
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDomain removes a domain; the next route sync deletes its Ingress.
func (s *Store) DeleteDomain(ctx context.Context, appID int64, hostname string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM app_domains WHERE app_id = $1 AND hostname = $2`, appID, hostname)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SaveDomainCheck stores the outcome of a verification run: status, routed,
// verified_by, error, verified_at and failing_since as given in d, and
// last_checked_at = now(). A domain deleted meanwhile returns ErrNotFound.
func (s *Store) SaveDomainCheck(ctx context.Context, d Domain) (Domain, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE app_domains SET status = $2, routed = $3, verified_by = $4, error = $5,
			verified_at = $6, failing_since = $7, last_checked_at = now()
		WHERE id = $1`, d.ID, d.Status, d.Routed, d.VerifiedBy, d.Error, d.VerifiedAt, d.FailingSince)
	if err != nil {
		return d, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return d, ErrNotFound
	}
	return s.getDomain(ctx, `dm.id = $1`, d.ID)
}

// HasProduction reports whether the app has a production alias, i.e.
// whether its custom domains have a deployment to route to.
func (s *Store) HasProduction(ctx context.Context, appID int64) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM aliases WHERE app_id = $1 AND kind = $2)`, appID, AliasProduction).Scan(&ok)
	return ok, err
}
