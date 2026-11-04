package fakegh

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// The protocol of the fake runner in testdata/fakerunner. The runner gets
// the server URL, its ID, and its token from the JIT config. GitHub learns
// that a job started when a runner takes it, so the server sends JobStarted
// when the runner gets its job and JobCompleted when it reports the result.

func (s *Server) runnerRoutes() {
	s.mux.HandleFunc("POST /_fake/runners/{id}/session", s.runnerConnect)
	s.mux.HandleFunc("DELETE /_fake/runners/{id}/session", s.runnerDisconnect)
	s.mux.HandleFunc("GET /_fake/runners/{id}/job", s.runnerJob)
	s.mux.HandleFunc("POST /_fake/runners/{id}/heartbeat", s.runnerHeartbeat)
	s.mux.HandleFunc("POST /_fake/runners/{id}/complete", s.runnerComplete)
	s.mux.HandleFunc("GET /_fake/logs/{job}", s.serveLog)
}

// jitConfig encodes a JIT config in the shape of the real one: base64 JSON
// that maps the runner config file names to base64 file contents.
func jitConfig(serverURL string, rn *runner, workFolder string) string {
	files := map[string]any{
		".runner": map[string]any{
			"agentId":    rn.id,
			"agentName":  rn.name,
			"poolId":     rn.set.id,
			"poolName":   rn.set.name,
			"serverUrl":  serverURL,
			"gitHubUrl":  serverURL + "/" + rn.set.scope,
			"workFolder": cmp.Or(workFolder, "_work"),
			"ephemeral":  true,
		},
		".credentials": map[string]any{
			"scheme": "OAuthAccessToken",
			"data":   map[string]string{"token": rn.token},
		},
	}
	encoded := map[string]string{}
	for name, v := range files {
		b, _ := json.Marshal(v)
		encoded[name] = base64.StdEncoding.EncodeToString(b)
	}
	b, _ := json.Marshal(encoded)
	return base64.StdEncoding.EncodeToString(b)
}

// runnerOf returns the runner of the path if the request has its token, or
// writes the error. It counts as a sign of life.
func (s *Server) runnerOf(w http.ResponseWriter, r *http.Request) *runner {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rn := s.runners[id]
	if rn == nil {
		restError(w, http.StatusNotFound, "The runner registration was deleted.")
		return nil
	}
	if r.Header.Get("Authorization") != "Bearer "+rn.token {
		restError(w, http.StatusUnauthorized, "Bad credentials")
		return nil
	}
	rn.seen = time.Now()
	return rn
}

func (s *Server) runnerConnect(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rn := s.runnerOf(w, r)
	if rn == nil {
		return
	}
	rn.online = true
	s.notify()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        rn.id,
		"name":      rn.name,
		"heartbeat": max(s.cfg.RunnerTimeout.Seconds()/4, 0.2), // seconds
	})
}

// runnerDisconnect ends the session of a runner. A runner that shuts down
// during its job cancels the job, as on GitHub. A runner that dies fails
// its job (watchRunners).
func (s *Server) runnerDisconnect(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rn := s.runnerOf(w, r)
	if rn == nil {
		return
	}
	rn.online = false
	if rn.job != nil {
		s.complete(rn.job, ResultCanceled)
	}
	s.notify()
	w.WriteHeader(http.StatusNoContent)
}

// runnerJob is the long poll of an idle runner. It returns the job of the
// runner, or 204 when none comes within PollTimeout.
func (s *Server) runnerJob(w http.ResponseWriter, r *http.Request) {
	timeout := time.NewTimer(s.cfg.PollTimeout)
	defer timeout.Stop()
	for {
		s.mu.Lock()
		rn := s.runnerOf(w, r)
		if rn == nil {
			s.mu.Unlock()
			return
		}
		rn.online = true
		var body any
		if j := s.assign(rn); j != nil {
			body = map[string]any{
				"request_id":  j.requestID,
				"timeline_id": j.uuid,
				"name":        j.run.spec.Name,
				"steps":       j.run.spec.Steps,
				"canceled":    j.cancel,
			}
		}
		wake := s.wake
		s.mu.Unlock()
		if body != nil {
			writeJSON(w, http.StatusOK, body)
			return
		}
		select {
		case <-wake:
		case <-timeout.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-s.closed:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// runnerHeartbeat keeps a busy runner alive and tells it if its job was
// canceled.
func (s *Server) runnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rn := s.runnerOf(w, r); rn != nil {
		writeJSON(w, http.StatusOK, map[string]bool{"canceled": rn.job != nil && rn.job.cancel})
	}
}

// runnerComplete takes the result of the job, as the runner prints it.
func (s *Server) runnerComplete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rn := s.runnerOf(w, r)
	if rn == nil {
		return
	}
	var in struct {
		RequestID int64  `json:"request_id"`
		Result    string `json:"result"`
	}
	if err := readJSON(r, &in); err != nil {
		restError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	result, ok := map[string]string{"Succeeded": ResultSucceeded, "Failed": ResultFailed, "Canceled": ResultCanceled}[in.Result]
	if !ok {
		restError(w, http.StatusBadRequest, "The result must be Succeeded, Failed, or Canceled.")
		return
	}
	if rn.job == nil || rn.job.requestID != in.RequestID {
		restError(w, http.StatusConflict, "The runner does not run this job.")
		return
	}
	s.complete(rn.job, result)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveLog(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := strconv.ParseInt(r.PathValue("job"), 10, 64)
	j := s.jobByID(id)
	if j == nil || j.log == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(j.log)
}

// jobLog is the log that GitHub keeps for a job that ran: a UTF-8 byte
// order mark, then lines with a timestamp of seven decimal places.
func jobLog(j *job, version string) []byte {
	var b bytes.Buffer
	b.WriteString("\xef\xbb\xbf")
	t := j.started
	line := func(text string) {
		fmt.Fprintf(&b, "%s %s\n", earliest(t, j.finished).UTC().Format("2006-01-02T15:04:05.0000000Z"), text)
	}
	line("Current runner version: '" + version + "'")
	line("Runner name: '" + j.runnerName + "'")
	line("Runner group name: 'Default'")
	line("Machine name: '" + j.runnerName + "'")
	for _, st := range j.run.spec.Steps {
		line("##[group]" + st.Name)
		line("##[endgroup]")
		for _, l := range st.Lines {
			line(l)
		}
		t = t.Add(time.Duration(st.Sleep * float64(time.Second)))
		if st.Fail {
			line("##[error]Process completed with exit code 1.")
			break
		}
	}
	t = j.finished
	if j.result == ResultCanceled {
		line("##[error]The operation was canceled.")
	}
	line("Cleaning up orphan processes")
	return b.Bytes()
}

type archive struct {
	once sync.Once
	data []byte
	sum  string // SHA-256 in hex
	err  error
}

// runnerArchive returns the release archive of the fake runner.
func (s *Server) runnerArchive() ([]byte, string, error) {
	a := &s.archive
	a.once.Do(func() {
		if s.cfg.RunnerDir == "" {
			a.err = errors.New("no runner bundle")
			return
		}
		a.data, a.err = tarGz(s.cfg.RunnerDir)
		sum := sha256.Sum256(a.data)
		a.sum = hex.EncodeToString(sum[:])
	})
	return a.data, a.sum, a.err
}

// tarGz packs dir without the folders that a runner writes. Same files
// give the same bytes.
func tarGz(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		if d.IsDir() && (rel == "_diag" || rel == "_work") {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel), ModTime: time.Unix(0, 0), Mode: 0o644, Typeflag: tar.TypeReg, Size: info.Size()}
		switch {
		case d.IsDir():
			hdr.Name, hdr.Mode, hdr.Typeflag, hdr.Size = hdr.Name+"/", 0o755, tar.TypeDir, 0
		case !info.Mode().IsRegular():
			return nil
		case info.Mode()&0o111 != 0:
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil || d.IsDir() {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", dir, err)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
