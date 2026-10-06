package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/nisagwn/paas/internal/api"
	"github.com/nisagwn/paas/internal/auth"
	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 20: the "Süreçler" section of the app page. It is loaded by htmx
// after the page (the cluster state takes a few API calls) and shows the
// production deployment's web process, workers and cron jobs. Members
// change worker replicas and start cron runs; both go through the same
// code as the JSON API (api.ScaleProcess, api.RunCron).

var processStateLabels = map[string]string{
	"running":   "Çalışıyor",
	"starting":  "Başlıyor",
	"stopped":   "Durduruldu",
	"sleeping":  "Uykuda",
	"crashing":  "Çöküyor",
	"missing":   "Kümede yok",
	"unknown":   "Bilinmiyor",
	"succeeded": "Başarılı",
	"failed":    "Başarısız",
}

var processStateClasses = map[string]string{
	"running":   "s-ready",
	"succeeded": "s-ready",
	"starting":  "s-deploying",
	"crashing":  "s-failed",
	"failed":    "s-failed",
}

var processKindLabels = map[string]string{
	process.KindWeb:    "Web",
	process.KindWorker: "Arka plan",
	process.KindCron:   "Zamanlanmış",
}

// processMessages translate the messages of api.ScaleProcess and api.RunCron.
var processMessages = map[string]string{
	"the web process is not scaled by hand: it runs one replica per deployment and scales to zero when idle": "Web süreci elle ölçeklenmez: her deploy'da bir kopya çalışır, boştayken uyutulur.",
	"the app has no production deployment yet":                                                               "Projenin henüz canlı bir deploy'u yok.",
	"a run of this cron job is still active":                                                                 "Bu işin önceki çalışması hâlâ sürüyor.",
	"the cron job is not in the cluster; redeploy the production deployment":                                 "Bu iş kümede yok; canlı deploy'u yeniden yayınla.",
	"running cron jobs needs PAAS_DEPLOYER=kubernetes":                                                       "Zamanlanmış işleri çalıştırmak için PAAS_DEPLOYER=kubernetes gerekir.",
	"replicas must be between 0 and 10":                                                                      "Kopya sayısı 0 ile 10 arasında olmalı.",
}

var processPatterns = []struct {
	re *regexp.Regexp
	tr string
}{
	{regexp.MustCompile(`(?i)^the production deployment has no worker process "(.+)"$`), "Canlı deploy'da %q adlı bir arka plan süreci yok."},
	{regexp.MustCompile(`(?i)^the production deployment has no cron job "(.+)"$`), "Canlı deploy'da %q adlı bir zamanlanmış iş yok."},
	{regexp.MustCompile(`(?i)^invalid process name "(.*)"$`), "Geçersiz süreç adı: %q."},
	{regexp.MustCompile(`(?i)^replicas saved, but applying them failed \(retried automatically\): (.+)$`), "Kopya sayısı kaydedildi ama kümeye uygulanamadı; otomatik olarak tekrar denenecek: %s"},
}

func init() {
	funcs["pstate"] = label(processStateLabels)
	funcs["pclass"] = func(s string) string { return processStateClasses[s] }
	funcs["pkind"] = label(processKindLabels)
	for k, v := range processMessages {
		normMessages[norm(k)] = v
	}
	patterns = append(patterns, processPatterns...)
}

// processesData is the data of the "processes" partial.
type processesData struct {
	App      store.App
	CanWrite bool
	// HasProduction: there is a production deployment to show.
	HasProduction bool
	View          api.ProcessesView
	// Busy: something is starting or running; the partial polls.
	Busy  bool
	Error string
	Flash string
}

func (s *Server) processesData(ctx context.Context, app store.App) (processesData, error) {
	v := processesData{App: app}
	caller, _ := auth.From(ctx)
	role, err := s.Auth.TeamRole(ctx, caller, app.TeamID)
	if err != nil {
		return v, err
	}
	v.CanWrite = store.RoleAllows(role, store.RoleMember)
	d, err := s.Store.ProductionAliasTarget(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	v.HasProduction = true
	if v.View, err = api.ProcessesFor(ctx, s.Store, s.Processes, app, d); err != nil {
		return v, err
	}
	for _, p := range v.View.Processes {
		if p.State == "starting" || p.State == "crashing" {
			v.Busy = true
		}
	}
	for _, c := range v.View.Crons {
		if c.Running > 0 {
			v.Busy = true
		}
	}
	return v, nil
}

func (s *Server) renderProcesses(w http.ResponseWriter, r *http.Request, app store.App, status int, errMsg, flash string) {
	v, err := s.processesData(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	v.Error, v.Flash = tr(errMsg), flash
	s.partial(w, r, status, "processes", v, "")
}

// processesPartial is loaded by the app page (and polled while busy).
func (s *Server) processesPartial(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	s.renderProcesses(w, r, app, http.StatusOK, "", "")
}

// processResult answers a process form: the partial for htmx, otherwise
// the app page (with the error) or a redirect back to the section.
func (s *Server) processResult(w http.ResponseWriter, r *http.Request, app store.App, status int, err error, flash string) {
	if status == http.StatusInternalServerError {
		s.internalError(w, r, err)
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	switch {
	case isHTMX(r):
		s.renderProcesses(w, r, app, status, msg, flash)
	case msg != "":
		s.renderTabOf(w, r, app, status, tabOverview, msg, nil)
	default:
		http.Redirect(w, r, "/apps/"+app.Name+"#processes", http.StatusSeeOther)
	}
}

func (s *Server) scaleProcess(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	proc := r.PostFormValue("process")
	n, err := strconv.Atoi(r.PostFormValue("replicas"))
	if err != nil {
		s.processResult(w, r, app, http.StatusBadRequest, errors.New("Kopya sayısı 0 ile 10 arasında olmalı."), "")
		return
	}
	status, err := api.ScaleProcess(r.Context(), s.Store, s.Router, app, proc, &n)
	if err == nil {
		s.Log.Info("process scaled", "app", app.Name, "process", proc, "replicas", n, "via", "web")
	}
	s.processResult(w, r, app, status, err, proc+" için kopya sayısı "+strconv.Itoa(n)+" yapıldı.")
}

func (s *Server) runCron(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	cron := r.PostFormValue("cron")
	job, status, err := api.RunCron(r.Context(), s.Store, s.Processes, app, cron)
	if err == nil {
		s.Log.Info("cron run", "app", app.Name, "cron", cron, "job", job, "via", "web")
	}
	s.processResult(w, r, app, status, err, cron+" başlatıldı.")
}

// processNames lists the processes of a deployment for the runtime log
// selector of the deployment page; nil when there is only web.
func (s *Server) processNames(ctx context.Context, d store.Deployment) []string {
	set, err := s.Store.DeploymentProcesses(ctx, d.ID)
	if err != nil {
		s.Log.Error("web: process set", "deployment", d.ID, "err", err)
		return nil
	}
	if set.Empty() {
		return nil
	}
	return set.Names()
}
