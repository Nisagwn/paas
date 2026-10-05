package api

import (
	"errors"
	"net/http"

	"github.com/nisagwn/paas/internal/build"
	"github.com/nisagwn/paas/internal/store"
)

// Faz 16: project settings (Vercel's "Build & Development Settings"). They
// apply to deployments built after the change.

type settingsView struct {
	store.BuildSettings
	// DetectedFramework is what the latest build used, e.g. "Next.js".
	DetectedFramework string `json:"detected_framework"`
	// Frameworks are the accepted values of "framework" besides "".
	Frameworks []build.Framework `json:"frameworks"`
}

// settingsRequest: a missing field keeps its value, "" resets it to the
// detected default.
type settingsRequest struct {
	RootDirectory   *string `json:"root_directory"`
	Framework       *string `json:"framework"`
	InstallCommand  *string `json:"install_command"`
	BuildCommand    *string `json:"build_command"`
	StartCommand    *string `json:"start_command"`
	OutputDirectory *string `json:"output_directory"`
	NodeVersion     *string `json:"node_version"`
}

func (r settingsRequest) apply(s store.BuildSettings) store.BuildSettings {
	for _, f := range []struct {
		from *string
		to   *string
	}{
		{r.RootDirectory, &s.RootDirectory}, {r.Framework, &s.Framework},
		{r.InstallCommand, &s.InstallCommand}, {r.BuildCommand, &s.BuildCommand},
		{r.StartCommand, &s.StartCommand}, {r.OutputDirectory, &s.OutputDirectory},
		{r.NodeVersion, &s.NodeVersion},
	} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	return s
}

func (s *Server) settingsView(w http.ResponseWriter, r *http.Request, app store.App, b store.BuildSettings) {
	detected, err := s.Store.DetectedFramework(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsView{BuildSettings: b, DetectedFramework: detected, Frameworks: build.Frameworks})
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	b, err := s.Store.GetBuildSettings(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.settingsView(w, r, app, b)
}

// putSettings merges the given fields into the app's build settings.
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req settingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cur, err := s.Store.GetBuildSettings(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	next, err := build.NormalizeSettings(req.apply(cur))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := s.Store.UpdateBuildSettings(r.Context(), app.ID, next)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("build settings updated", "app", app.Name, "framework", saved.Framework, "root", saved.RootDirectory)
	s.settingsView(w, r, app, saved)
}
