package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 21: the rollout panel of the app page (templates/rollout.html):
// progress through the canary steps, both sides' 5xx rate and p95, the
// actions (Hemen tamamla / Durdur / Sürdür / Geri al) and the settings form.

var rolloutStateLabels = map[string]string{
	store.RolloutRunning:    "Sürüyor",
	store.RolloutPaused:     "Duraklatıldı",
	store.RolloutPromoted:   "Tamamlandı",
	store.RolloutRolledBack: "Geri alındı",
	store.RolloutAborted:    "Durduruldu",
	store.RolloutSuperseded: "Yerini yenisi aldı",
}

var rolloutModeLabels = map[string]string{
	store.RolloutInstant: "Anında",
	store.RolloutGuarded: "İzlemeli geçiş",
	store.RolloutCanary:  "Kademeli (canary)",
}

var rolloutActionLabels = map[string]string{
	"start": "Başladı", "advance": "İlerledi", "promote": "Tamamlandı", "rollback": "Geri alındı",
	"abort": "Durduruldu", "pause": "Duraklatıldı", "resume": "Sürdürüldü", "supersede": "Yerini yenisi aldı",
}

// rolloutBadge maps a state to an existing badge colour class.
var rolloutBadge = map[string]string{
	store.RolloutRunning:    "s-building",
	store.RolloutPaused:     "s-verified",
	store.RolloutPromoted:   "s-ready",
	store.RolloutRolledBack: "s-failed",
	store.RolloutAborted:    "s-failed",
	store.RolloutSuperseded: "s-retired",
}

func init() {
	funcs["rolloutState"] = label(rolloutStateLabels)
	funcs["rolloutMode"] = label(rolloutModeLabels)
	funcs["rolloutAction"] = label(rolloutActionLabels)
	funcs["rolloutBadge"] = label(rolloutBadge)
	funcs["errPct"] = func(s store.RolloutSample) string {
		if s.Requests == 0 {
			return "—"
		}
		return fmt.Sprintf("%%%.1f", s.ErrorPct())
	}
	funcs["p95"] = func(s store.RolloutSample) string {
		if s.P95Ms == nil {
			return "—"
		}
		return fmt.Sprintf("%.0f ms", *s.P95Ms)
	}
	funcs["seconds"] = func(n int64) string { return shortDuration(time.Duration(n) * time.Second) }
	funcs["stepsText"] = store.FormatSteps
	funcs["minutes"] = func(sec int) string { return strconv.FormatFloat(float64(sec)/60, 'f', -1, 64) }
}

type rolloutStep struct {
	Weight        int
	Done, Current bool
}

// rolloutPanel is the data of the rollout panel.
type rolloutPanel struct {
	api.RolloutStatus
	StepsText string
	// Steps of the active canary, for the progress bar.
	Steps []rolloutStep
	// Recent are the finished rollouts (at most 5).
	Recent []api.RolloutView
	// ByDeployment: the latest rollout state of each new deployment, for
	// the deployment list.
	ByDeployment map[int64]store.Rollout
}

func (s *Server) rolloutPanel(ctx context.Context, app store.App) (*rolloutPanel, error) {
	st, err := api.LoadRolloutStatus(ctx, s.Store, app, s.Scheme, s.Domain, time.Now())
	if err != nil {
		return nil, err
	}
	p := &rolloutPanel{RolloutStatus: st, StepsText: store.FormatSteps(st.Settings.Steps), ByDeployment: map[int64]store.Rollout{}}
	for _, r := range st.Recent {
		if _, seen := p.ByDeployment[r.ToDeploymentID]; !seen {
			p.ByDeployment[r.ToDeploymentID] = r.Rollout
		}
		if !r.Active() && len(p.Recent) < 5 {
			p.Recent = append(p.Recent, r)
		}
	}
	if a := st.Active; a != nil && a.Mode == store.RolloutCanary {
		for i, w := range a.Settings.Steps {
			p.Steps = append(p.Steps, rolloutStep{Weight: w, Done: i < a.Step, Current: i == a.Step})
		}
	}
	return p, nil
}

// rolloutAction serves the panel's buttons.
func (s *Server) rolloutAction(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	switch action {
	case store.ActionPromote, store.ActionPause, store.ActionResume, store.ActionRollback, store.ActionAbort:
	default:
		s.errorPage(w, r, http.StatusNotFound, "Sayfa bulunamadı.")
		return
	}
	me, _ := auth.From(r.Context())
	_, err := api.RolloutAction(r.Context(), s.Store, s.Router, app, action, me.Login)
	switch {
	case errors.Is(err, api.ErrRouteSync):
		s.Log.Error("rollout action: route sync", "app", app.Name, "err", err)
		s.renderTabOf(w, r, app, http.StatusBadGateway, tabOverview, "Kaydedildi ama yönlendirme güncellenemedi; otomatik olarak tekrar denenecek.", nil)
		return
	case errors.Is(err, store.ErrNotFound):
		s.renderTabOf(w, r, app, http.StatusNotFound, tabOverview, "Süren bir yayın yok; sayfa güncel olmayabilir.", nil)
		return
	case errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrStale):
		s.renderTabOf(w, r, app, http.StatusConflict, tabOverview, "Yayının durumu bu sırada değişti; sayfayı yenileyip tekrar dene.", nil)
		return
	case errors.Is(err, store.ErrRetired), errors.Is(err, store.ErrNotReady):
		s.renderTabOf(w, r, app, http.StatusConflict, tabOverview, "Hedef deploy artık çalışmıyor.", nil)
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("rollout action", "app", app.Name, "action", action, "via", "web")
	redirect(w, r, "/apps/"+app.Name+"?ok=rollout#rollout")
}

// rolloutSettings saves the settings form.
func (s *Server) rolloutSettings(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	cur, err := s.Store.GetRolloutSettings(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	next, msg := parseRolloutForm(r, cur)
	if msg == "" {
		if err := next.Validate(); err != nil {
			msg = "Geçersiz ayar: " + err.Error()
		}
	}
	if msg != "" {
		s.renderTabOf(w, r, app, http.StatusBadRequest, tabOverview, msg, nil)
		return
	}
	if _, err := s.Store.SetRolloutSettings(r.Context(), app.ID, next); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.Log.Info("rollout settings", "app", app.Name, "mode", next.Mode, "via", "web")
	redirect(w, r, "/apps/"+app.Name+"?ok=rollout-settings#rollout")
}

// parseRolloutForm reads the form; durations are in minutes there.
func parseRolloutForm(r *http.Request, s store.RolloutSettings) (store.RolloutSettings, string) {
	s.Mode = r.PostFormValue("mode")
	steps, err := store.ParseSteps(r.PostFormValue("steps"))
	if err != nil {
		return s, "Adımlar virgülle ayrılmış yüzdeler olmalı, örneğin 10,50,100."
	}
	s.Steps = steps
	num := func(field string, dst *float64) bool {
		v := strings.TrimSpace(strings.ReplaceAll(r.PostFormValue(field), ",", "."))
		if v == "" {
			return true
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return false
		}
		*dst = f
		return true
	}
	stepMin, guardMin := float64(s.StepSeconds)/60, float64(s.GuardSeconds)/60
	p95, minReq := float64(s.MaxP95Ms), float64(s.MinRequests)
	for _, f := range []struct {
		name string
		dst  *float64
	}{
		{"step_minutes", &stepMin}, {"guard_minutes", &guardMin}, {"max_error_pct", &s.MaxErrorPct},
		{"max_error_increase_pct", &s.MaxErrorIncreasePct}, {"max_p95_ms", &p95}, {"max_p95_factor", &s.MaxP95Factor},
		{"min_requests", &minReq},
	} {
		if !num(f.name, f.dst) {
			return s, "Sayısal alanlara yalnızca sayı yazılabilir."
		}
	}
	s.StepSeconds, s.GuardSeconds = int(stepMin*60), int(guardMin*60)
	s.MaxP95Ms, s.MinRequests = int(p95), int(minReq)
	return s, ""
}
