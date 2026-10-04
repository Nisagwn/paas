package store

import "context"

// MigrateUntil applies migrations up to and including version until.
func (s *Store) MigrateUntil(ctx context.Context, until string) error { return s.migrate(ctx, until) }

// Exec runs raw SQL, for tests that need rows an older schema would hold.
func (s *Store) Exec(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}
