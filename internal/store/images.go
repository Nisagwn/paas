package store

import (
	"context"
	"database/sql"
	"errors"
)

// Faz 30: registry retention follows deployment retention. The registry
// keeps exactly the images of the deployments the platform keeps (queued,
// building, deploying, ready: live, rollback targets, active previews);
// the cleanup deletes the image of a finished (retired, failed, canceled)
// deployment once nothing kept references it any more.
//
// Several deployments can reference one manifest: a redeploy or promotion
// with image reuse copies the reference, and two builds can produce the same
// digest. Deleting a manifest by digest removes every tag pointing at it, so
// the decision is made per digest, never per deployment.

// imageMatch selects the deployments referencing the manifest digest $3 in
// repository $2 ("<host>/<repo>", from a pinned "<repo>:<tag>@<digest>" or
// "<repo>@<digest>"), or holding the reference $4 exactly (an image pushed
// without a reported digest).
const imageMatch = `image <> '' AND (
	(left(image, length($2) + 1) IN ($2 || ':', $2 || '@') AND $3 <> '' AND right(image, length($3) + 1) = '@' || $3)
	OR image = $4)`

// ReleaseImage reports whether the manifest digest in repository name may be
// deleted from the registry on behalf of finished deployment id, whose
// stored reference is image. It may when id is retired, failed or canceled
// and no kept deployment references the same manifest; every finished
// deployment sharing it is then marked released (image_released_at), so a
// later redeploy builds instead of reusing a deleted image.
//
// All rows sharing the manifest are locked FOR UPDATE before the check, and
// CopyDeployment reads its source FOR SHARE: a concurrent copy either
// commits first and is seen here as a reference, or sees the release.
// Repeating the call after a failed registry delete is safe.
func (s *Store) ReleaseImage(ctx context.Context, id int64, name, digest, image string) (bool, error) {
	if name == "" || (digest == "" && image == "") {
		return false, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		SELECT id FROM deployments WHERE id = $1 OR (`+imageMatch+`)
		ORDER BY id FOR UPDATE`, id, name, digest, image); err != nil {
		return false, err
	}
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM deployments WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if !finishedStatus(status) {
		return false, nil
	}
	// A new statement sees rows committed while we waited for the locks.
	var kept bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM deployments
			WHERE id <> $1 AND status IN ('queued', 'building', 'deploying', 'ready') AND (`+imageMatch+`))`,
		id, name, digest, image).Scan(&kept); err != nil {
		return false, err
	}
	if kept {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE deployments SET image_released_at = COALESCE(image_released_at, now())
		WHERE (id = $1 OR (`+imageMatch+`)) AND status IN ('retired', 'failed', 'canceled')`,
		id, name, digest, image); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func finishedStatus(status string) bool {
	return status == StatusRetired || status == StatusFailed || status == StatusCanceled
}

// ImageReleased reports whether deployment id's image was released.
func (s *Store) ImageReleased(ctx context.Context, id int64) (bool, error) {
	var released bool
	err := s.db.QueryRowContext(ctx,
		`SELECT image_released_at IS NOT NULL FROM deployments WHERE id = $1`, id).Scan(&released)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return released, err
}
