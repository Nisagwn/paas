package api

import "net/http"

// Faz 11: per-app scale-to-zero setting. Preview deployments always sleep
// when idle (PAAS_SCALE_TO_ZERO_AFTER > 0); production only on opt-in.

type scaleToZeroView struct {
	Production bool `json:"production"`
}

func (s *Server) getScaleToZero(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	on, err := s.Store.ScaleToZeroProduction(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scaleToZeroView{Production: on})
}

// putScaleToZero takes {"production": true|false}. Turning it off wakes a
// sleeping production deployment on the scaler's next check.
func (s *Server) putScaleToZero(w http.ResponseWriter, r *http.Request) {
	app, ok := s.lookupApp(w, r)
	if !ok {
		return
	}
	var req struct {
		Production *bool `json:"production"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Production == nil {
		writeError(w, http.StatusBadRequest, `body must be {"production": true|false}`)
		return
	}
	if err := s.Store.SetScaleToZeroProduction(r.Context(), app.ID, *req.Production); err != nil {
		s.internalError(w, err)
		return
	}
	s.Log.Info("scale to zero setting", "app", app.Name, "production", *req.Production)
	writeJSON(w, http.StatusOK, scaleToZeroView{Production: *req.Production})
}
