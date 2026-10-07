package deploy

import (
	"encoding/json"
	"strings"
)

// Faz 22: the shell scripts of add-on pods and Jobs. They run in the
// official postgres image (busybox ash with pipefail, psql, pg_dump,
// pg_restore) and take every value from the environment: identifiers and
// passwords reach SQL only through psql variables (:"db", :'pw'), which
// psql quotes, never through string concatenation.
//
// A Job reports its result in its termination message: JSON on success
// (CopyReport, BackupReport); on failure the pod's last log lines
// (terminationMessagePolicy FallbackToLogsOnError).

// postgresInitScript runs once, when the data directory is initialized
// (docker-entrypoint-initdb.d): it creates the application role and its
// database and closes the other databases to everyone but their owners and
// the superuser.
const postgresInitScript = `#!/bin/sh
set -eu
psql -X -v ON_ERROR_STOP=1 -q --username "$POSTGRES_USER" --dbname postgres -v pw="$APP_PASSWORD" <<'SQL'
CREATE ROLE app LOGIN PASSWORD :'pw';
CREATE DATABASE app OWNER app;
REVOKE ALL ON DATABASE app FROM PUBLIC;
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
SQL
`

// scriptPrelude is shared by the Job scripts.
const scriptPrelude = `set -eu
set -o pipefail
export PGPASSWORD="$ADMIN_PASSWORD" PGUSER=postgres PGCONNECT_TIMEOUT=10
q() { psql -X -v ON_ERROR_STOP=1 -q "$@"; }
`

// copyScript (re)creates the branch database and its owner role and, in
// copy mode, streams pg_dump of production into pg_restore. It never uses
// CREATE DATABASE ... TEMPLATE, which needs production to have no
// connections. A copy above MAX_BYTES, or one that would fill more than
// 80 % of the volume, falls back to an empty database. Anonymization runs
// in the copy only: the generated column rules as the superuser with
// triggers off (session_replication_role = replica), then the member's
// own statements in a session of the branch role (not SET ROLE, which the
// statements could reset), sent with -c: one request to the server,
// without psql's backslash commands.
const copyScript = scriptPrelude + `
mode="$MODE"; warning=""
source_size=$(psql -X -Atq -d "$SOURCE_DB" -c "SELECT pg_database_size(current_database())")
used=$(psql -X -Atq -d postgres -c "SELECT COALESCE(sum(pg_database_size(datname)), 0) FROM pg_database")
if [ "$mode" = copy ]; then
  if [ "$source_size" -gt "$MAX_BYTES" ]; then
    mode=empty; warning=size
  elif [ $((used + source_size)) -gt $((CAPACITY_BYTES / 10 * 8)) ]; then
    mode=empty; warning=disk
  fi
fi
echo "==> database $TARGET_DB ($mode); production is $source_size bytes"
q -d postgres -v db="$TARGET_DB" -v pw="$BRANCH_PASSWORD" <<'SQL'
DROP DATABASE IF EXISTS :"db" WITH (FORCE);
DROP ROLE IF EXISTS :"db";
CREATE ROLE :"db" LOGIN PASSWORD :'pw';
CREATE DATABASE :"db" OWNER :"db";
REVOKE ALL ON DATABASE :"db" FROM PUBLIC;
SQL
snapshot=$(date -u +%Y-%m-%dT%H:%M:%SZ)
if [ "$mode" = copy ]; then
  echo "==> pg_dump $SOURCE_DB | pg_restore $TARGET_DB"
  pg_dump -Fc --no-owner --no-acl -d "$SOURCE_DB" |
    pg_restore --no-owner --no-acl --exit-on-error --role="$TARGET_DB" -d "$TARGET_DB"
  if [ -n "${ANON_SQL:-}" ]; then
    echo "==> anonymization rules"
    printf '%s\n' "$ANON_SQL" | q --single-transaction -d "$TARGET_DB"
  fi
  if [ -n "${USER_SQL:-}" ]; then
    echo "==> anonymization statements (as $TARGET_DB)"
    PGPASSWORD="$BRANCH_PASSWORD" psql -X -v ON_ERROR_STOP=1 -q -U "$TARGET_DB" -d "$TARGET_DB" -c "$USER_SQL"
  fi
fi
size=$(psql -X -Atq -d "$TARGET_DB" -c "SELECT pg_database_size(current_database())")
printf '{"mode":"%s","warning":"%s","snapshot":"%s","size":%s,"source_size":%s}' \
  "$mode" "$warning" "$snapshot" "$size" "$source_size" > /dev/termination-log
echo "==> done: $size bytes"
`

// dropScript drops a branch database and its role.
const dropScript = scriptPrelude + `
q -d postgres -v db="$TARGET_DB" <<'SQL'
DROP DATABASE IF EXISTS :"db" WITH (FORCE);
DROP ROLE IF EXISTS :"db";
SQL
echo "==> dropped $TARGET_DB"
`

// rotateScript sets the application role's new password. Open connections
// stay; new ones need the new password.
const rotateScript = scriptPrelude + `
q -d postgres -v pw="$NEW_PASSWORD" <<'SQL'
ALTER ROLE app WITH PASSWORD :'pw';
SQL
echo "==> password of role app changed"
`

// backupScript dumps production into the backup volume (atomically: a
// partial file is renamed when complete), keeps the newest KEEP dumps and
// reports what is left.
const backupScript = scriptPrelude + `
cd /backups
find . -maxdepth 1 -name '*.partial' -mmin +180 -exec rm -f {} +
f="$JOB_NAME.dump"
echo "==> pg_dump app > $f"
pg_dump -Fc -d app -f "$f.partial"
mv "$f.partial" "$f"
size=$(stat -c %s "$f")
ls -1t -- *.dump | tail -n +$((KEEP + 1)) | xargs -r rm -f --
kept=""
for k in $(ls -1t -- *.dump); do kept="$kept${kept:+,}\"$k\""; done
printf '{"file":"%s","size":%s,"kept":[%s]}' "$f" "$size" "$kept" > /dev/termination-log
echo "==> $f: $size bytes"
`

// restoreScript replaces production with a dump in one transaction: on any
// error nothing changes. Objects are recreated owned by the app role.
const restoreScript = scriptPrelude + `
f="/backups/$BACKUP_FILE"
if [ ! -f "$f" ]; then
  echo "backup file $BACKUP_FILE is no longer on the backup volume" >&2
  exit 1
fi
export PGOPTIONS="-c lock_timeout=60s"
echo "==> pg_restore $BACKUP_FILE into app"
pg_restore --clean --if-exists --no-owner --no-acl --role=app --single-transaction -d app "$f"
echo "==> restored"
`

// CopyReport is the termination message of a successful copy Job.
type CopyReport struct {
	// Mode is what was done: copy, or empty (requested, or a fallback).
	Mode string `json:"mode"`
	// Warning is "size" (production above the copy limit) or "disk" (the
	// volume would be over 80 % full) when a copy fell back to empty.
	Warning    string `json:"warning"`
	Snapshot   string `json:"snapshot"`
	Size       int64  `json:"size"`
	SourceSize int64  `json:"source_size"`
}

// BackupReport is the termination message of a successful backup Job.
type BackupReport struct {
	File string   `json:"file"`
	Size int64    `json:"size"`
	Kept []string `json:"kept"`
}

// ParseCopyReport reads a copy Job's termination message.
func ParseCopyReport(msg string) (CopyReport, bool) {
	var r CopyReport
	err := json.Unmarshal([]byte(strings.TrimSpace(msg)), &r)
	return r, err == nil && r.Mode != ""
}

// ParseBackupReport reads a backup Job's termination message.
func ParseBackupReport(msg string) (BackupReport, bool) {
	var r BackupReport
	err := json.Unmarshal([]byte(strings.TrimSpace(msg)), &r)
	return r, err == nil && r.File != ""
}
