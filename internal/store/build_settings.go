package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
)

// Faz 16: per-app build settings. Every field is optional; the empty string
// means "use what the platform detects". Validation lives in
// build.NormalizeSettings, which knows the framework presets.

// BuildSettings are an app's overrides of the detected build.
type BuildSettings struct {
	// RootDirectory is the subdirectory of the repository that is built
	// (monorepos); "" is the repository root.
	RootDirectory string `json:"root_directory"`
	// Framework forces a preset or project kind; "" detects it.
	Framework      string `json:"framework"`
	InstallCommand string `json:"install_command"`
	BuildCommand   string `json:"build_command"`
	StartCommand   string `json:"start_command"`
	// OutputDirectory is the directory a static build is served from,
	// relative to RootDirectory.
	OutputDirectory string `json:"output_directory"`
	// NodeVersion selects the node image, e.g. "20" → node:20-alpine.
	NodeVersion string `json:"node_version"`
	// UpdatedAt is nil while the app has never saved settings.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// GetBuildSettings returns the app's build settings; an app that never saved
// any gets the zero value (everything detected).
func (s *Store) GetBuildSettings(ctx context.Context, appID int64) (BuildSettings, error) {
	var b BuildSettings
	var updated time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT root_directory, framework, install_command, build_command, start_command,
			output_directory, node_version, updated_at
		FROM app_build_settings WHERE app_id = $1`, appID).Scan(
		&b.RootDirectory, &b.Framework, &b.InstallCommand, &b.BuildCommand, &b.StartCommand,
		&b.OutputDirectory, &b.NodeVersion, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return BuildSettings{}, nil
	}
	if err != nil {
		return BuildSettings{}, err
	}
	b.UpdatedAt = &updated
	return b, nil
}

// UpdateBuildSettings replaces the app's build settings and returns them as
// stored. They apply to deployments built afterwards. ErrNotFound: no such app.
func (s *Store) UpdateBuildSettings(ctx context.Context, appID int64, b BuildSettings) (BuildSettings, error) {
	var updated time.Time
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO app_build_settings (app_id, root_directory, framework, install_command, build_command,
			start_command, output_directory, node_version, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (app_id) DO UPDATE SET
			root_directory = EXCLUDED.root_directory, framework = EXCLUDED.framework,
			install_command = EXCLUDED.install_command, build_command = EXCLUDED.build_command,
			start_command = EXCLUDED.start_command, output_directory = EXCLUDED.output_directory,
			node_version = EXCLUDED.node_version, updated_at = now()
		RETURNING updated_at`,
		appID, b.RootDirectory, b.Framework, b.InstallCommand, b.BuildCommand, b.StartCommand,
		b.OutputDirectory, b.NodeVersion).Scan(&updated)
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23503" { // foreign_key_violation
		return BuildSettings{}, ErrNotFound
	}
	if err != nil {
		return BuildSettings{}, err
	}
	b.UpdatedAt = &updated
	return b, nil
}

// SetFramework records the framework a deployment's build used.
func (s *Store) SetFramework(ctx context.Context, id int64, framework string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET framework = $2 WHERE id = $1`, id, framework)
	return err
}

// DetectedFramework is the framework of the app's most recent deployment
// whose build got that far, or "" if there is none.
func (s *Store) DetectedFramework(ctx context.Context, appID int64) (string, error) {
	var f string
	err := s.db.QueryRowContext(ctx, `
		SELECT framework FROM deployments WHERE app_id = $1 AND framework <> ''
		ORDER BY id DESC LIMIT 1`, appID).Scan(&f)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return f, err
}
