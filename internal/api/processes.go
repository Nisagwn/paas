package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/process"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 20: process types (web / worker / cron).
//
//	GET  /api/apps/{name}/processes[?deployment=<id>]  viewer
//	PUT  /api/apps/{name}/processes/{proc}             member  {"replicas": n | null}
//	POST /api/apps/{name}/crons/{cron}/run             member
//	GET  /api/apps/{name}/deployments/{id}/runtime-logs?process=<name>
//
// Without ?deployment the production deployment is shown: its workers and
// crons are the ones that run (process.PlanFor). A replica change is an
// app-wide override of paas.yaml for production; it is applied by the
// route reconcile at once (Router.SyncApp) and survives new deployments.

// ProcessCluster reads and drives a deployment's processes
// (deploy.Kubernetes). The Server finds it on RuntimeLogs; nil (dry-run
// deployer) shows the configured processes without cluster state.
type ProcessCluster interface {
	ProcessStatus(ctx context.Context, d store.Deployment) (process.Status, error)
	RunCron(ctx context.Context, d store.Deployment, cron string) (string, error)
	ProcessLogs(ctx context.Context, d store.Deployment, proc string, follow bool, tail int64, w io.Writer) error
}

var _ ProcessCluster = (*deploy.Kubernetes)(nil)

func (s *Server) processCluster() ProcessCluster {
	c, _ := s.RuntimeLogs.(ProcessCluster)
	return c
}

// ProcessesView is the process set of one deployment with its cluster state.
type ProcessesView struct {
	DeploymentID int64 `json:"deployment_id"`
	// Production: the production alias points at the deployment, so its
	// workers and crons are the ones that run.
	Production bool `json:"production"`
	// Source: paas.yaml, paas.yml, paas.json, Procfile or "" (none).
	Source    string        `json:"source"`
	Processes []ProcessView `json:"processes"`
	Crons     []CronView    `json:"crons"`
	// StatusError: the cluster state could not be read; desired and ready
	// counts are missing.
	StatusError string `json:"status_error,omitempty"`
}

// ProcessView is the web process or a worker.
type ProcessView struct {
	Name string `json:"name"`
	Type string `json:"type"` // web or worker
	// Command as configured; empty for web means the detected start command.
	Command string `json:"command"`
	// Replicas from paas.yaml/Procfile (workers); Override is the app's
	// replica setting, which wins in production.
	Replicas int  `json:"replicas"`
	Override *int `json:"override,omitempty"`
	Previews bool `json:"previews"`
	// Cluster state (process.ReplicaStatus).
	Desired int32  `json:"desired"`
	Ready   int32  `json:"ready"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
}

// CronView is a cron job with its last run.
type CronView struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	Previews bool   `json:"previews"`
	process.CronStatus
}

// ProcessesFor builds the view of deployment d of app. cluster may be nil.
func ProcessesFor(ctx context.Context, st *store.Store, cluster ProcessCluster, app store.App, d store.Deployment) (ProcessesView, error) {
	set, err := st.DeploymentProcesses(ctx, d.ID)
	if err != nil {
		return ProcessesView{}, err
	}
	overrides, err := st.ProcessReplicas(ctx, app.ID)
	if err != nil {
		return ProcessesView{}, err
	}
	v := ProcessesView{DeploymentID: d.ID, Source: set.Source, Processes: []ProcessView{}, Crons: []CronView{}}
	if prod, err := st.ProductionAliasTarget(ctx, app.ID); err == nil {
		v.Production = prod.ID == d.ID
	} else if !errors.Is(err, store.ErrNotFound) {
		return v, err
	}

	var status process.Status
	switch {
	case cluster == nil:
		v.StatusError = "cluster state needs PAAS_DEPLOYER=kubernetes"
	case d.Status == store.StatusRetired:
		v.StatusError = "the deployment is retired: its processes were removed from the cluster"
	default:
		if status, err = cluster.ProcessStatus(ctx, d); err != nil {
			v.StatusError = "reading the cluster state failed: " + err.Error()
		}
	}
	missing := process.ReplicaStatus{State: "missing"}
	if cluster == nil || v.StatusError != "" {
		missing.State = "unknown"
	}

	if set.HasWeb() {
		pv := ProcessView{Name: process.Web, Type: process.KindWeb, Command: set.WebCommand, Replicas: 1, Previews: true}
		rs := missing
		if status.Web != nil {
			rs = *status.Web
		}
		pv.Desired, pv.Ready, pv.State, pv.Reason = rs.Desired, rs.Ready, rs.State, rs.Reason
		v.Processes = append(v.Processes, pv)
	}
	for _, w := range set.Workers {
		pv := ProcessView{Name: w.Name, Type: process.KindWorker, Command: w.Command, Replicas: w.Replicas, Previews: w.Previews}
		if n, ok := overrides[w.Name]; ok {
			pv.Override = &n
		}
		rs, ok := status.Workers[w.Name]
		if !ok {
			rs = missing
		}
		pv.Desired, pv.Ready, pv.State, pv.Reason = rs.Desired, rs.Ready, rs.State, rs.Reason
		v.Processes = append(v.Processes, pv)
	}
	for _, c := range set.Crons {
		v.Crons = append(v.Crons, CronView{Name: c.Name, Schedule: c.Schedule, Command: c.Command, Previews: c.Previews,
			CronStatus: status.Crons[c.Name]})
	}
	return v, nil
}

// processDeployment resolves ?deployment=<id>, else the production deployment.
func (s *Server) processDeployment(w http.ResponseWriter, r *http.Request, app store.App) (store.Deployment, bool) {
	if v := r.URL.Query().Get("deployment"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "deployment must be a deployment id")
			return store.Deployment{}, false
		}
		return s.appDeployment(w, r, app, id)
	}
	return s.productionDeployment(w, r, app)
}

func (s *Server) productionDeployment(w http.ResponseWriter, r *http.Request, app store.App) (store.Deployment, bool) {
	d, err := s.Store.ProductionAliasTarget(r.Context(), app.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "the app has no production deployment yet; pass ?deployment=<id> for another one")
		return d, false
	}
	if err != nil {
		s.internalError(w, err)
		return d, false
	}
	return d, true
}

func (s *Server) getProcesses(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	d, ok := s.processDeployment(w, r, app)
	if !ok {
		return
	}
	v, err := ProcessesFor(r.Context(), s.Store, s.processCluster(), app, d)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ScaleProcess validates and stores a replica override of a worker of the
// app's production deployment and applies it. The error text is meant for
// the user; status is the HTTP status of the failure.
func ScaleProcess(ctx context.Context, st *store.Store, router Router, app store.App, proc string, replicas *int) (status int, err error) {
	switch {
	case proc == process.Web:
		return http.StatusBadRequest, errors.New("the web process is not scaled by hand: it runs one replica " +
			"per deployment and scales to zero when idle")
	case !process.ValidName(proc):
		return http.StatusBadRequest, fmt.Errorf("invalid process name %q", proc)
	case replicas != nil && (*replicas < 0 || *replicas > process.MaxReplicas):
		return http.StatusBadRequest, fmt.Errorf("replicas must be between 0 and %d", process.MaxReplicas)
	}
	prod, err := st.ProductionAliasTarget(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound, errors.New("the app has no production deployment yet")
	}
	if err != nil {
		return http.StatusInternalServerError, err
	}
	set, err := st.DeploymentProcesses(ctx, prod.ID)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if _, ok := set.Worker(proc); !ok && replicas != nil {
		return http.StatusNotFound, fmt.Errorf("the production deployment has no worker process %q", proc)
	}
	if err := st.SetProcessReplicas(ctx, app.ID, proc, replicas); err != nil {
		return http.StatusInternalServerError, err
	}
	if router != nil {
		if err := router.SyncApp(ctx, app.Name); err != nil {
			return http.StatusBadGateway, errors.New("replicas saved, but applying them failed (retried automatically): " + err.Error())
		}
	}
	return http.StatusOK, nil
}

func (s *Server) putProcess(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req struct {
		Replicas json.RawMessage `json:"replicas"`
	}
	const shape = `body must be {"replicas": <0-10>} or {"replicas": null} to use paas.yaml again`
	if err := decodeJSON(r, &req); err != nil || len(req.Replicas) == 0 {
		writeError(w, http.StatusBadRequest, shape)
		return
	}
	var replicas *int
	if !bytes.Equal(bytes.TrimSpace(req.Replicas), []byte("null")) {
		var n int
		if err := json.Unmarshal(req.Replicas, &n); err != nil {
			writeError(w, http.StatusBadRequest, shape)
			return
		}
		replicas = &n
	}
	proc := r.PathValue("proc")
	status, err := ScaleProcess(r.Context(), s.Store, s.Router, app, proc, replicas)
	if status == http.StatusInternalServerError {
		s.internalError(w, err)
		return
	}
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.Log.Info("process scaled", "app", app.Name, "process", proc, "replicas", replicas)
	d, ok := s.productionDeployment(w, r, app)
	if !ok {
		return
	}
	v, err := ProcessesFor(r.Context(), s.Store, s.processCluster(), app, d)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// RunCron starts a run of a cron job of the app's production deployment.
func RunCron(ctx context.Context, st *store.Store, cluster ProcessCluster, app store.App, cron string) (job string, status int, err error) {
	if cluster == nil {
		return "", http.StatusNotImplemented, errors.New("running cron jobs needs PAAS_DEPLOYER=kubernetes")
	}
	prod, err := st.ProductionAliasTarget(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", http.StatusNotFound, errors.New("the app has no production deployment yet")
	}
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	set, err := st.DeploymentProcesses(ctx, prod.ID)
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	if _, ok := set.Cron(cron); !ok {
		return "", http.StatusNotFound, fmt.Errorf("the production deployment has no cron job %q", cron)
	}
	job, err = cluster.RunCron(ctx, prod, cron)
	switch {
	case errors.Is(err, deploy.ErrCronRunning):
		return "", http.StatusConflict, errors.New("a run of this cron job is still active")
	case errors.Is(err, deploy.ErrNotFound):
		return "", http.StatusConflict, errors.New("the cron job is not in the cluster; redeploy the production deployment")
	case err != nil:
		return "", http.StatusBadGateway, err
	}
	return job, http.StatusAccepted, nil
}

func (s *Server) runCron(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	cron := r.PathValue("cron")
	job, status, err := RunCron(r.Context(), s.Store, s.processCluster(), app, cron)
	if status == http.StatusInternalServerError {
		s.internalError(w, err)
		return
	}
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	s.Log.Info("cron run", "app", app.Name, "cron", cron, "job", job)
	writeJSON(w, status, map[string]string{"cron": cron, "job": job})
}

// processLogTarget checks ?process= of the runtime logs before streaming:
// "" and "web" are the web process; any other name must be a worker or
// cron of the deployment.
func (s *Server) processLogTarget(w http.ResponseWriter, r *http.Request, d store.Deployment) (string, bool) {
	proc := r.URL.Query().Get("process")
	if proc == "" || proc == process.Web {
		return proc, true
	}
	if !process.ValidName(proc) {
		writeError(w, http.StatusBadRequest, "invalid process name")
		return "", false
	}
	if s.processCluster() == nil {
		writeError(w, http.StatusNotImplemented, "process logs need PAAS_DEPLOYER=kubernetes")
		return "", false
	}
	set, err := s.Store.DeploymentProcesses(r.Context(), d.ID)
	if err != nil {
		s.internalError(w, err)
		return "", false
	}
	_, isWorker := set.Worker(proc)
	_, isCron := set.Cron(proc)
	if !isWorker && !isCron {
		writeError(w, http.StatusNotFound, fmt.Sprintf("deployment %d has no process %q", d.ID, proc))
		return "", false
	}
	return proc, true
}

// streamRuntimeLogs reads the web process through RuntimeLogs and the
// others through the ProcessCluster.
func (s *Server) streamRuntimeLogs(ctx context.Context, d store.Deployment, proc string, follow bool, tail int64, w io.Writer) error {
	if proc == "" || proc == process.Web {
		return s.RuntimeLogs.RuntimeLogs(ctx, d, follow, tail, w)
	}
	return s.processCluster().ProcessLogs(ctx, d, proc, follow, tail, w)
}
