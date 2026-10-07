package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Addons configures managed databases (Faz 22, internal/addons). The
// per-add-on settings (plan, preview mode, anonymization, backups kept)
// live in the database.
type Addons struct {
	// Interval of the reconcile loop: PAAS_ADDON_INTERVAL, default 10s,
	// at least 2s.
	Interval time.Duration
	// StorageClass of the volumes: PAAS_ADDON_STORAGE_CLASS, default
	// local-path (k3s); "default" uses the cluster's default class.
	StorageClass string
	// Images: PAAS_ADDON_POSTGRES_IMAGE (postgres:16-alpine) and
	// PAAS_ADDON_REDIS_IMAGE (redis:7-alpine).
	PostgresImage, RedisImage string
	// BackupSchedule of the daily backup CronJobs, cron syntax in UTC:
	// PAAS_ADDON_BACKUP_SCHEDULE, default "0 3 * * *".
	BackupSchedule string
	// CopyMaxBytes: a production database larger than this is not copied
	// into previews; they get an empty database and a warning.
	// PAAS_ADDON_COPY_MAX_SIZE, Kubernetes quantity, default 5Gi.
	CopyMaxBytes int64
	// CopyTimeout bounds one copy Job: PAAS_ADDON_COPY_TIMEOUT, default 10m.
	CopyTimeout time.Duration
}

// LoadAddons reads the add-on settings from the environment.
func LoadAddons() (Addons, error) {
	a := Addons{
		Interval:       10 * time.Second,
		StorageClass:   getenv("PAAS_ADDON_STORAGE_CLASS", "local-path"),
		PostgresImage:  getenv("PAAS_ADDON_POSTGRES_IMAGE", "postgres:16-alpine"),
		RedisImage:     getenv("PAAS_ADDON_REDIS_IMAGE", "redis:7-alpine"),
		BackupSchedule: getenv("PAAS_ADDON_BACKUP_SCHEDULE", "0 3 * * *"),
		CopyMaxBytes:   5 << 30,
		CopyTimeout:    10 * time.Minute,
	}
	for _, d := range []struct {
		key string
		dst *time.Duration
		min time.Duration
	}{{"PAAS_ADDON_INTERVAL", &a.Interval, 2 * time.Second}, {"PAAS_ADDON_COPY_TIMEOUT", &a.CopyTimeout, time.Minute}} {
		if v := os.Getenv(d.key); v != "" {
			n, err := time.ParseDuration(v)
			if err != nil || n < d.min {
				return a, fmt.Errorf("%s must be a duration of at least %s", d.key, d.min)
			}
			*d.dst = n
		}
	}
	if v := os.Getenv("PAAS_ADDON_COPY_MAX_SIZE"); v != "" {
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			return a, fmt.Errorf("PAAS_ADDON_COPY_MAX_SIZE must be a size such as 5Gi or 500Mi")
		}
		a.CopyMaxBytes = q.Value()
	}
	if len(strings.Fields(a.BackupSchedule)) != 5 {
		return a, fmt.Errorf("PAAS_ADDON_BACKUP_SCHEDULE must have five cron fields (e.g. \"0 3 * * *\")")
	}
	return a, nil
}
