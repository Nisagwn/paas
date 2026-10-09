package notify

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// Constructors of the events the control plane emits. Texts are Turkish,
// short, and end with a link to the page that explains more.

func envLabel(target string) string {
	if target == store.EnvProduction {
		return "Production"
	}
	return "Önizleme"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}

// clip shortens s to n runes with an ellipsis.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func deploymentLink(id int64) string { return "/deployments/" + strconv.FormatInt(id, 10) }

func commitLabel(d store.Deployment) string {
	c := naming.ShortSHA(d.CommitSHA)
	if m := firstLine(d.CommitMessage); m != "" {
		c += " " + clip(m, 72)
	}
	return c
}

// Deployment is the event of a finished deployment (worker.Result).
func Deployment(d store.Deployment, r worker.Result) Event {
	e := Event{
		Key: fmt.Sprintf("deployment:%d:%s", d.ID, r.Status), AppID: d.AppID, App: d.AppName, DeploymentID: d.ID,
		Link: deploymentLink(d.ID), LinkText: "Deploy'u aç",
		Fields: []Field{
			{Name: "Proje", Value: d.AppName},
			{Name: "Ortam", Value: envLabel(d.Target)},
			{Name: "Branch", Value: d.Branch},
			{Name: "Commit", Value: commitLabel(d)},
		},
		Data: map[string]any{
			"deployment_id": d.ID, "status": r.Status, "commit_sha": d.CommitSHA, "branch": d.Branch,
			"target": d.Target, "origin": d.Origin, "deployment_url": r.URL,
		},
	}
	switch r.Status {
	case store.StatusReady:
		e.Type, e.Severity = DeploymentReady, SeveritySuccess
		e.Title = d.AppName + ": deploy hazır"
		e.Text = fmt.Sprintf("%s@%s %s ortamında yayında.", d.Branch, naming.ShortSHA(d.CommitSHA), strings.ToLower(envLabel(d.Target)))
		e.Fields = append(e.Fields, Field{Name: "Adres", Value: r.URL})
		if r.ProductionURL != "" {
			e.Fields = append(e.Fields, Field{Name: "Production", Value: r.ProductionURL})
			e.Data["production_url"] = r.ProductionURL
		}
	case store.StatusCanceled:
		e.Type, e.Severity = DeploymentCanceled, SeverityWarning
		e.Title = d.AppName + ": deploy iptal edildi"
		e.Text = fmt.Sprintf("%s@%s deploy'u tamamlanmadan iptal edildi.", d.Branch, naming.ShortSHA(d.CommitSHA))
	default:
		e.Type, e.Severity = DeploymentFailed, SeverityError
		e.Title = d.AppName + ": deploy başarısız"
		e.Text = fmt.Sprintf("%s@%s deploy'u başarısız oldu.", d.Branch, naming.ShortSHA(d.CommitSHA))
		if r.Error != "" {
			e.Fields = append(e.Fields, Field{Name: "Hata", Value: clip(firstLine(r.Error), 300)})
			e.Data["error"] = clip(r.Error, 2000)
		}
		e.LinkText = "Logları aç"
	}
	return e
}

// Reasons of a production change.
const (
	ReasonRollback = "rollback"
	ReasonPromote  = "promote"
	ReasonCanary   = "canary"
	ReasonRollout  = "rollout" // a user's rollout action (promote / rollback)
)

var reasonLabels = map[string]string{
	ReasonRollback: "elle geri alındı",
	ReasonPromote:  "production'a taşındı",
	ReasonCanary:   "kademeli yayın tamamlandı",
	ReasonRollout:  "yayın elle sonuçlandırıldı",
}

// ProductionChange is the event of the production alias moving to d.
// key makes a retried emission idempotent; "" is unique per call.
func ProductionChange(d store.Deployment, reason, by, key string) Event {
	why := reasonLabels[reason]
	if why == "" {
		why = reason
	}
	e := Event{
		Type: ProductionChanged, Key: key, AppID: d.AppID, App: d.AppName, DeploymentID: d.ID,
		Severity: SeverityInfo, Link: deploymentLink(d.ID), LinkText: "Deploy'u aç",
		Title: d.AppName + ": production değişti",
		Text:  fmt.Sprintf("Production artık #%d deploy'unu (%s@%s) gösteriyor: %s.", d.ID, d.Branch, naming.ShortSHA(d.CommitSHA), why),
		Fields: []Field{
			{Name: "Proje", Value: d.AppName},
			{Name: "Deploy", Value: "#" + strconv.FormatInt(d.ID, 10)},
			{Name: "Commit", Value: commitLabel(d)},
		},
		Data: map[string]any{"deployment_id": d.ID, "reason": reason, "commit_sha": d.CommitSHA, "branch": d.Branch},
	}
	if reason == ReasonCanary {
		e.Severity = SeveritySuccess
	}
	if by != "" {
		e.Fields = append(e.Fields, Field{Name: "Yapan", Value: "@" + by})
		e.Data["by"] = by
	}
	return e
}

// RolloutProductionTarget is the deployment a user's rollout action moved
// production to (canary promoted → the new one; guarded rolled back → the
// previous one), or 0 when production did not move.
func RolloutProductionTarget(r store.Rollout) int64 {
	switch {
	case r.State == store.RolloutPromoted && r.Mode == store.RolloutCanary:
		return r.ToDeploymentID
	case r.State == store.RolloutRolledBack && r.Mode == store.RolloutGuarded && r.FromDeploymentID != nil:
		return *r.FromDeploymentID
	}
	return 0
}

// Rollout is the event of a rollout the controller finished (Faz 21): an
// automatic rollback or abort, or a completed canary (production changed).
// ok is false for outcomes nobody is notified about.
func Rollout(r store.Rollout, d store.Deployment) (Event, bool) {
	from := "önceki deploy"
	if r.FromDeploymentID != nil {
		from = "#" + strconv.FormatInt(*r.FromDeploymentID, 10)
	}
	e := Event{
		Type: RolloutRolledBack, Key: fmt.Sprintf("rollout:%d:%s", r.ID, r.State), AppID: r.AppID, App: r.AppName,
		DeploymentID: r.ToDeploymentID, Link: deploymentLink(r.ToDeploymentID), LinkText: "Deploy'u aç",
		Fields: []Field{
			{Name: "Proje", Value: r.AppName},
			{Name: "Yeni deploy", Value: "#" + strconv.FormatInt(r.ToDeploymentID, 10)},
			{Name: "Mod", Value: map[string]string{store.RolloutCanary: "Canary", store.RolloutGuarded: "İzlemeli geçiş"}[r.Mode]},
		},
		Data: map[string]any{"rollout_id": r.ID, "mode": r.Mode, "state": r.State, "reason": r.Reason,
			"to_deployment_id": r.ToDeploymentID, "from_deployment_id": r.FromDeploymentID},
	}
	if r.Reason != "" {
		e.Fields = append(e.Fields, Field{Name: "Neden", Value: clip(r.Reason, 300)})
	}
	switch r.State {
	case store.RolloutRolledBack:
		e.Severity = SeverityError
		e.Title = r.AppName + ": yayın otomatik geri alındı"
		if r.Mode == store.RolloutGuarded {
			e.Text = fmt.Sprintf("Metrikler bozulduğu için production %s deploy'una döndü.", from)
		} else {
			e.Text = fmt.Sprintf("Metrikler bozulduğu için canary trafiği %%0'a çekildi; production %s deploy'unda kaldı.", from)
		}
	case store.RolloutAborted:
		e.Severity = SeverityWarning
		e.Title = r.AppName + ": yayın durduruldu"
		e.Text = fmt.Sprintf("Yayın tamamlanamadı; production %s deploy'unda kaldı.", from)
	case store.RolloutPromoted:
		if r.Mode != store.RolloutCanary {
			return Event{}, false // guarded: production moved when the deployment became ready
		}
		pc := ProductionChange(d, ReasonCanary, "", fmt.Sprintf("rollout:%d:promoted", r.ID))
		pc.AppID, pc.App = r.AppID, r.AppName
		return pc, true
	default:
		return Event{}, false
	}
	return e, true
}

// Addon failure kinds.
const (
	AddonProvision = "provision"
	AddonCopy      = "copy"
	AddonBackup    = "backup"
	AddonRestore   = "restore"
)

var addonWhat = map[string]string{
	AddonProvision: "hazırlanamadı",
	AddonCopy:      "branch kopyası alınamadı",
	AddonBackup:    "yedeği alınamadı",
	AddonRestore:   "yedekten geri yüklenemedi",
}

// AddonFailure is the event of a managed database problem (Faz 22). key
// identifies the occurrence (e.g. the backup job's name).
func AddonFailure(a store.Addon, kind, detail, key string) Event {
	e := Event{
		Type: AddonFailed, Key: "addon:" + strconv.FormatInt(a.ID, 10) + ":" + kind + ":" + key,
		AppID: a.AppID, App: a.AppName, Severity: SeverityError,
		Link: "/apps/" + a.AppName + "/databases", LinkText: "Veritabanlarını aç",
		Title: fmt.Sprintf("%s: %q veritabanı %s", a.AppName, a.Name, addonWhat[kind]),
		Fields: []Field{
			{Name: "Proje", Value: a.AppName},
			{Name: "Eklenti", Value: a.Name + " (" + a.Kind + ")"},
		},
		Data: map[string]any{"addon": a.Name, "kind": a.Kind, "failure": kind},
	}
	if detail != "" {
		e.Text = clip(firstLine(detail), 400)
		e.Data["detail"] = clip(detail, 2000)
	}
	return e
}

// DomainFailure is the event of a custom domain that failed verification
// (Faz 12): a routed domain whose DNS broke, or a new one still unverified
// after a day.
func DomainFailure(d store.Domain, stuck bool) Event {
	e := Event{
		Type: DomainFailed, AppID: d.AppID, App: d.AppName, Severity: SeverityWarning,
		Link: "/apps/" + d.AppName + "/settings#custom-domains", LinkText: "Alan adlarını aç",
		Title:  d.Hostname + " alan adı doğrulanamadı",
		Fields: []Field{{Name: "Proje", Value: d.AppName}, {Name: "Alan adı", Value: d.Hostname}},
		Data:   map[string]any{"hostname": d.Hostname, "status": d.Status, "error": d.Error},
	}
	if stuck {
		e.Key = "domain:" + strconv.FormatInt(d.ID, 10) + ":pending"
		e.Text = "Alan adı eklendikten bir gün sonra hâlâ doğrulanamadı; DNS kaydını kontrol et."
	} else {
		since := d.CreatedAt
		if d.FailingSince != nil {
			since = *d.FailingSince
		}
		e.Key = "domain:" + strconv.FormatInt(d.ID, 10) + ":failing:" + strconv.FormatInt(since.Unix(), 10)
		e.Severity = SeverityError
		e.Text = "DNS kaydı artık doğrulanmıyor; düzeltilmezse alan adı bir süre sonra yayından kalkar."
	}
	if d.Error != "" {
		e.Fields = append(e.Fields, Field{Name: "Ayrıntı", Value: clip(d.Error, 300)})
	}
	return e
}

// CertificateExpiry is the event of a custom domain certificate that has
// not been renewed although it expires soon.
func CertificateExpiry(d store.Domain, notAfter, now time.Time) Event {
	days := int(notAfter.Sub(now).Hours() / 24)
	return Event{
		Type: CertExpiring, Key: "cert:" + d.Hostname + ":" + strconv.FormatInt(notAfter.Unix(), 10),
		AppID: d.AppID, App: d.AppName, Severity: SeverityWarning,
		Link: "/apps/" + d.AppName + "/settings#custom-domains", LinkText: "Alan adlarını aç",
		Title: d.Hostname + " sertifikasının süresi doluyor",
		Text: fmt.Sprintf("Sertifika %s tarihinde sona eriyor (%d gün kaldı) ve henüz yenilenmedi; alan adının DNS kaydını ve cert-manager'ı kontrol et.",
			notAfter.UTC().Format("02.01.2006"), days),
		Fields: []Field{{Name: "Proje", Value: d.AppName}, {Name: "Alan adı", Value: d.Hostname},
			{Name: "Bitiş", Value: notAfter.UTC().Format("02.01.2006 15:04 UTC")}},
		Data: map[string]any{"hostname": d.Hostname, "not_after": notAfter.UTC(), "days_left": days},
	}
}

// Limit is the event of a team or app reaching a limit (plans and limits,
// Faz 23). appID 0 is team-wide. One event per limit, app and day unless
// the caller sets Key.
func Limit(teamID, appID int64, app, limit string, used, max int64, detail string, now time.Time) Event {
	e := Event{
		Type: LimitReached, TeamID: teamID, AppID: appID, App: app, Severity: SeverityWarning,
		Key:   "limit:" + limit + ":" + strconv.FormatInt(appID, 10) + ":" + now.UTC().Format("2006-01-02"),
		Title: "Limite ulaşıldı: " + limit,
		Text:  detail,
		Fields: []Field{{Name: "Limit", Value: limit},
			{Name: "Kullanım", Value: strconv.FormatInt(used, 10) + " / " + strconv.FormatInt(max, 10)}},
		Data: map[string]any{"limit": limit, "used": used, "max": max},
	}
	if app != "" {
		e.Fields = append([]Field{{Name: "Proje", Value: app}}, e.Fields...)
		e.Link, e.LinkText = "/apps/"+app, "Projeyi aç"
	}
	if e.Text == "" {
		e.Text = fmt.Sprintf("%s limiti doldu (%d / %d); yeni işlemler reddedilebilir.", limit, used, max)
	}
	return e
}
