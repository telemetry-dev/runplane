package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/sandbox"
)

// newRoutes returns the routes that the agent serves to the primary.
func (a *agent) newRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", a.startSandbox)
	mux.HandleFunc("GET /v1/sandboxes", a.listSandboxes)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", a.stopSandbox)
	mux.HandleFunc("GET /v1/sandboxes/{id}/logs", a.sandboxLogs)
	mux.HandleFunc("POST /v1/sandboxes/{id}/hold", a.holdSandbox)
	mux.HandleFunc("GET /v1/sandboxes/{id}/shell", a.sandboxShell)
	mux.HandleFunc("GET /v1/capacity", a.readCapacity)
	mux.HandleFunc("GET /v1/host", a.readHost)
	mux.HandleFunc("POST /v1/images/ensure", a.ensureImage)
	mux.HandleFunc("POST /v1/images/gc", a.gcImages)
	mux.HandleFunc("GET /v1/mirror/stats", a.mirrorStats)
	mux.HandleFunc("POST /v1/runners/ensure", a.ensureRunner)
	mux.HandleFunc("POST /v1/runners/prune", a.pruneRunners)
	mux.HandleFunc("POST /v1/settings", a.configure)
	mux.HandleFunc("POST /v1/probe", a.probe)
	mux.HandleFunc("POST /v1/update", a.update)
	return mux
}

func (a *agent) startSandbox(w http.ResponseWriter, r *http.Request) {
	var spec sandbox.Spec
	if decode(w, r, &spec) {
		writeDone(w, a.Node.Driver().Start(r.Context(), spec))
	}
}

func (a *agent) listSandboxes(w http.ResponseWriter, r *http.Request) {
	list, err := a.Node.Driver().List(r.Context())
	if list == nil {
		list = []sandbox.Sandbox{}
	}
	writeJSON(w, list, err)
}

func (a *agent) stopSandbox(w http.ResponseWriter, r *http.Request) {
	writeDone(w, a.Node.Driver().Stop(r.Context(), r.PathValue("id")))
}

func (a *agent) sandboxLogs(w http.ResponseWriter, r *http.Request) {
	follow := r.URL.Query().Get("follow") == "1"
	rc, err := a.Node.Driver().Logs(r.Context(), r.PathValue("id"), follow)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Send the header now: a quiet log can take minutes for its first line.
	flush := http.NewResponseController(w).Flush
	flush()
	buf := make([]byte, 32<<10)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flush()
		}
		if err != nil {
			return
		}
	}
}

func (a *agent) holdSandbox(w http.ResponseWriter, r *http.Request) {
	var req holdRequest
	if decode(w, r, &req) {
		writeDone(w, a.Node.Driver().Hold(r.Context(), r.PathValue("id"), req.Until))
	}
}

// sandboxShell upgrades the stream to a shell. See readFrames for the
// stream format.
func (a *agent) sandboxShell(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != shellProtocol {
		http.Error(w, "the shell needs Upgrade: "+shellProtocol, http.StatusBadRequest)
		return
	}
	rows, err1 := strconv.ParseUint(r.URL.Query().Get("rows"), 10, 16)
	cols, err2 := strconv.ParseUint(r.URL.Query().Get("cols"), 10, 16)
	if err := errors.Join(err1, err2); err != nil {
		http.Error(w, "bad terminal size: "+err.Error(), http.StatusBadRequest)
		return
	}
	term, err := a.Node.Driver().Shell(r.Context(), r.PathValue("id"), sandbox.TermSize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		writeError(w, err)
		return
	}
	defer term.Close()
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + shellProtocol + "\r\n\r\n")
	if err := brw.Flush(); err != nil {
		return
	}
	go func() {
		io.Copy(conn, term)
		conn.Close()
	}()
	if err := readFrames(brw.Reader, term); !errors.Is(err, io.EOF) {
		a.Logger.Debug("shell stream ended", "err", err)
	}
}

func (a *agent) readCapacity(w http.ResponseWriter, r *http.Request) {
	c, err := a.Node.Driver().Capacity(r.Context())
	writeJSON(w, c, err)
}

// readHost returns the host facts, and with doctor=1 also the doctor
// checks for min_free_disk_percent.
func (a *agent) readHost(w http.ResponseWriter, r *http.Request) {
	info, err := a.Node.Facts(r.Context())
	rep := hostReport{Info: info}
	if q := r.URL.Query(); err == nil && q.Get("doctor") == "1" {
		minFree, _ := strconv.ParseFloat(q.Get("min_free_disk_percent"), 64)
		rep.Doctor, err = a.Node.Doctor(r.Context(), minFree)
	}
	writeJSON(w, rep, err)
}

// ensureImage streams the progress as JSON lines. The last line has the
// image or the error.
func (a *agent) ensureImage(w http.ResponseWriter, r *http.Request) {
	var ref sandbox.ImageRef
	if !decode(w, r, &ref) {
		return
	}
	if ref.Ref == "" {
		http.Error(w, "no image ref", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flush := http.NewResponseController(w).Flush
	enc := json.NewEncoder(w)
	send := func(m imageMessage) {
		enc.Encode(m)
		flush()
	}
	img, err := a.Node.EnsureImage(r.Context(), ref.Ref, func(p image.Progress) { send(imageMessage{Progress: &p}) })
	if err != nil {
		send(imageMessage{Error: err.Error()})
		return
	}
	send(imageMessage{Image: &img})
}

func (a *agent) gcImages(w http.ResponseWriter, r *http.Request) {
	var req gcRequest
	if decode(w, r, &req) {
		writeDone(w, a.Node.GCImages(r.Context(), req.Keep))
	}
}

func (a *agent) pruneRunners(w http.ResponseWriter, r *http.Request) {
	var req gcRequest
	if decode(w, r, &req) {
		writeDone(w, a.Node.PruneRunners(r.Context(), req.Keep))
	}
}

func (a *agent) mirrorStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, a.Mirror(), nil)
}

func (a *agent) ensureRunner(w http.ResponseWriter, r *http.Request) {
	var req runnerRequest
	if !decode(w, r, &req) {
		return
	}
	b, err := a.Node.EnsureRunner(r.Context(), req.Version, req.Arch)
	writeJSON(w, b, err)
}

func (a *agent) configure(w http.ResponseWriter, r *http.Request) {
	var s sandbox.Settings
	if decode(w, r, &s) {
		writeDone(w, a.Node.Driver().Configure(r.Context(), s))
	}
}

func (a *agent) probe(w http.ResponseWriter, r *http.Request) {
	writeDone(w, a.Node.Driver().Probe(r.Context()))
}

// update replaces the binary. After the answer is out, Run returns
// ErrUpdated.
func (a *agent) update(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if !decode(w, r, &req) {
		return
	}
	if err := a.Node.Update(r.Context(), req.Version); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	http.NewResponseController(w).Flush()
	a.Logger.Info("binary updated, restarting", "version", req.Version)
	a.updateOnce.Do(func() { close(a.updated) })
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeDone(w http.ResponseWriter, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// writeError answers 404 for sandbox.ErrNotFound, which Session turns back
// into the error.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, sandbox.ErrNotFound) {
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}
