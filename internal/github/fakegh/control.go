package fakegh

import (
	"net/http"
	"strconv"
)

// The control API under /_fake/ gives Queue, CancelJob, Jobs, Runners, and
// ScaleSets to tests and demos outside the process.

func (s *Server) controlRoutes() {
	s.mux.HandleFunc("POST /_fake/jobs", s.postJob)
	s.mux.HandleFunc("GET /_fake/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Jobs())
	})
	s.mux.HandleFunc("POST /_fake/jobs/{id}/cancel", s.postCancel)
	s.mux.HandleFunc("GET /_fake/runners", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Runners())
	})
	s.mux.HandleFunc("GET /_fake/scale-sets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.ScaleSets())
	})
}

func (s *Server) postJob(w http.ResponseWriter, r *http.Request) {
	var spec JobSpec
	if err := readJSON(r, &spec); err != nil || spec.Repo == "" || len(spec.Labels) == 0 {
		restError(w, http.StatusBadRequest, "The job needs repo and labels.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"request_id": s.Queue(spec)})
}

func (s *Server) postCancel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if !s.CancelJob(id) {
		restError(w, http.StatusConflict, "The job does not exist or is complete.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
