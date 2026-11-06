package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/runplane/runplane/internal/image"
	"github.com/runplane/runplane/internal/sandbox"
)

// The agent protocol. The primary calls the operations of a Node on an
// agent, each as one HTTP request on a new yamux stream of the connection.
// The table below gives each operation its method, its path, the types of
// its request and its answer, and the code that serves it on the agent.
// Session calls the operations, and the agent serves each operation of the
// table. The agent sends its events and its cache requests the other way,
// to the primary.
var (
	opStart = newOp(http.MethodPost, "/v1/sandboxes", func(ctx context.Context, a *agent, spec sandbox.Spec) (none, error) {
		return none{}, a.Node.Driver().Start(ctx, spec)
	})

	opList = newOp(http.MethodGet, "/v1/sandboxes", func(ctx context.Context, a *agent, _ none) ([]sandbox.Sandbox, error) {
		list, err := a.Node.Driver().List(ctx)
		if list == nil {
			list = []sandbox.Sandbox{}
		}
		return list, err
	})

	opStop = newOp(http.MethodDelete, "/v1/sandboxes/{id}", func(ctx context.Context, a *agent, id sandboxID) (none, error) {
		return none{}, a.Node.Driver().Stop(ctx, string(id))
	})

	opHold = newOp(http.MethodPost, "/v1/sandboxes/{id}/hold", func(ctx context.Context, a *agent, req holdRequest) (none, error) {
		return none{}, a.Node.Driver().Hold(ctx, req.ID, req.Until)
	})

	opLogs = newStream(http.MethodGet, "/v1/sandboxes/{id}/logs", serveLogs)

	opShell = newStream(http.MethodGet, "/v1/sandboxes/{id}/shell", serveShell)

	opCapacity = newOp(http.MethodGet, "/v1/capacity", func(ctx context.Context, a *agent, _ none) (sandbox.Capacity, error) {
		return a.Node.Driver().Capacity(ctx)
	})

	opProbe = newOp(http.MethodPost, "/v1/probe", func(ctx context.Context, a *agent, _ none) (none, error) {
		return none{}, a.Node.Driver().Probe(ctx)
	})

	opConfigure = newOp(http.MethodPost, "/v1/settings", func(ctx context.Context, a *agent, s Settings) (none, error) {
		return none{}, a.Node.Configure(ctx, s)
	})

	// opHost answers the facts, or with Doctor the report of the doctor,
	// which has the facts too.
	opHost = newOp(http.MethodGet, "/v1/host", func(ctx context.Context, a *agent, q hostQuery) (hostReport, error) {
		if !q.Doctor {
			info, err := a.Node.Facts(ctx)
			return hostReport{Info: wireInfo(info)}, err
		}
		r, err := a.Node.Doctor(ctx, q.MinFreeDiskPercent)
		return hostReport{Info: wireInfo(r.Info), Doctor: wireChecks(r.Checks)}, err
	})

	opEnsureImage = newStream(http.MethodPost, "/v1/images/ensure", serveEnsureImage)

	opGCImages = newOp(http.MethodPost, "/v1/images/gc", func(ctx context.Context, a *agent, req keepRequest) (none, error) {
		return none{}, a.Node.GCImages(ctx, req.Keep)
	})

	opEnsureRunner = newOp(http.MethodPost, "/v1/runners/ensure", func(ctx context.Context, a *agent, req runnerRequest) (bundle, error) {
		b, err := a.Node.EnsureRunner(ctx, req.Version, req.Arch)
		return wireBundle(b), err
	})

	opPruneRunners = newOp(http.MethodPost, "/v1/runners/prune", func(ctx context.Context, a *agent, req keepRequest) (none, error) {
		return none{}, a.Node.PruneRunners(ctx, req.Keep)
	})

	opMirrorStats = newOp(http.MethodGet, "/v1/mirror/stats", func(ctx context.Context, a *agent, _ none) (mirrorStats, error) {
		m, err := a.Node.MirrorStats(ctx)
		return wireMirror(m), err
	})

	// opUpdate replaces the binary. The primary closes the connection when
	// it has the answer, and then Run returns ErrUpdated.
	opUpdate = newOp(http.MethodPost, "/v1/update", func(ctx context.Context, a *agent, req updateRequest) (none, error) {
		if err := a.Node.Update(ctx, req.Version); err != nil {
			return none{}, err
		}
		a.Logger.Info("binary updated, restarting", "version", req.Version)
		a.updateOnce.Do(func() { close(a.updated) })
		return none{}, nil
	})
)

// The paths of the primary that an agent calls.
const (
	// eventsPath takes the stream of the driver events and the host stats
	// of the agent, as JSON lines of streamItem.
	eventsPath = "/v1/events"
	// cachePath and the paths under it take the Actions cache requests of
	// the sandboxes on the agent.
	cachePath = "/v1/cache"
	// sandboxHeader names the sandbox of a cache request. The agent finds
	// the sandbox at the source address of the request.
	sandboxHeader = "X-Runplane-Sandbox"
)

// Settings are the settings that a server gets from the primary: the
// settings of its sandbox driver, and min_free_disk_percent for its
// registry mirror.
type Settings struct {
	sandbox.Settings
	MinFreeDiskPercent float64 `json:"min_free_disk_percent"`
}

// none is the request or the answer of an operation that has none. The
// answer none is 204 No Content.
type none struct{}

type (
	holdRequest struct {
		ID    string    `json:"-"`
		Until time.Time `json:"until"`
	}
	logsRequest struct {
		ID     string
		Follow bool
	}
	shellRequest struct {
		ID   string
		Size sandbox.TermSize
	}
	hostQuery struct {
		Doctor             bool
		MinFreeDiskPercent float64
	}
	hostReport struct {
		Info   hostInfo    `json:"info"`
		Doctor []hostCheck `json:"doctor,omitempty"`
	}
	runnerRequest struct {
		Version string `json:"version"`
		Arch    string `json:"arch"`
	}
	// keepRequest names the images (by digest) or the runner versions
	// that the agent keeps.
	keepRequest struct {
		Keep []string `json:"keep"`
	}
	updateRequest struct {
		Version string `json:"version"`
	}
	// imageMessage is one line of the answer of opEnsureImage. The last
	// line has Image or Error.
	imageMessage struct {
		Progress *progress         `json:"progress,omitempty"`
		Image    *sandbox.ImageRef `json:"image,omitempty"`
		Error    string            `json:"error,omitempty"`
	}
	// streamItem is one line of the stream to eventsPath.
	streamItem struct {
		Event *sandbox.Event `json:"event,omitempty"`
		Stats *hostStats     `json:"stats,omitempty"`
	}
)

// sandboxID is the request of an operation on one sandbox.
type sandboxID string

// A request that is a urlArgs goes in the URL: the {id} of the path and the
// query. The agent reads it back with urlParser. A request of a POST also
// goes as the JSON body, without its fields that have no JSON.
type (
	urlArgs interface {
		args() (id string, query url.Values)
	}
	urlParser interface {
		parse(id string, query url.Values) error
	}
)

func (id sandboxID) args() (string, url.Values) { return string(id), nil }

func (id *sandboxID) parse(v string, _ url.Values) error {
	*id = sandboxID(v)
	return nil
}

func (r holdRequest) args() (string, url.Values) { return r.ID, nil }

func (r *holdRequest) parse(id string, _ url.Values) error {
	r.ID = id
	return nil
}

func (r logsRequest) args() (string, url.Values) {
	if r.Follow {
		return r.ID, url.Values{"follow": {"1"}}
	}
	return r.ID, nil
}

func (r *logsRequest) parse(id string, q url.Values) error {
	r.ID, r.Follow = id, q.Get("follow") == "1"
	return nil
}

func (r shellRequest) args() (string, url.Values) {
	return r.ID, url.Values{"rows": {strconv.Itoa(int(r.Size.Rows))}, "cols": {strconv.Itoa(int(r.Size.Cols))}}
}

func (r *shellRequest) parse(id string, q url.Values) error {
	rows, err1 := strconv.ParseUint(q.Get("rows"), 10, 16)
	cols, err2 := strconv.ParseUint(q.Get("cols"), 10, 16)
	if err := errors.Join(err1, err2); err != nil {
		return fmt.Errorf("bad terminal size: %w", err)
	}
	r.ID, r.Size = id, sandbox.TermSize{Rows: uint16(rows), Cols: uint16(cols)}
	return nil
}

func (q hostQuery) args() (string, url.Values) {
	if !q.Doctor {
		return "", nil
	}
	return "", url.Values{"doctor": {"1"}, "min_free_disk_percent": {strconv.FormatFloat(q.MinFreeDiskPercent, 'g', -1, 64)}}
}

func (q *hostQuery) parse(_ string, v url.Values) error {
	q.Doctor = v.Get("doctor") == "1"
	if q.Doctor {
		q.MinFreeDiskPercent, _ = strconv.ParseFloat(v.Get("min_free_disk_percent"), 64)
	}
	return nil
}

type route[In any] struct {
	method, path string
}

// op is an operation whose answer is Out as JSON.
type op[In, Out any] struct {
	route[In]
	serve func(ctx context.Context, a *agent, in In) (Out, error)
}

// stream is an operation whose answer the agent writes while it goes: a
// log, a shell, or the progress of an image.
type stream[In any] struct {
	route[In]
	serve func(w http.ResponseWriter, r *http.Request, a *agent, in In)
}

// handlers holds the handler of each operation that newOp and newStream
// make. The agent serves all of them.
var handlers []handler

type handler struct {
	pattern string
	serve   func(a *agent) http.HandlerFunc
}

func newOp[In, Out any](method, path string, serve func(context.Context, *agent, In) (Out, error)) op[In, Out] {
	o := op[In, Out]{route[In]{method, path}, serve}
	handlers = append(handlers, handler{method + " " + path, o.handler})
	return o
}

func newStream[In any](method, path string, serve func(http.ResponseWriter, *http.Request, *agent, In)) stream[In] {
	s := stream[In]{route[In]{method, path}, serve}
	handlers = append(handlers, handler{method + " " + path, s.handler})
	return s
}

func (o op[In, Out]) handler(a *agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, err := o.read(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out, err := o.serve(r.Context(), a, in)
		if err != nil {
			writeError(w, err)
			return
		}
		if _, ok := any(out).(none); ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}
}

func (s stream[In]) handler(a *agent) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, err := s.read(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.serve(w, r, a, in)
	}
}

func (rt route[In]) request(ctx context.Context, in In) (*http.Request, error) {
	path, query := rt.path, url.Values(nil)
	if a, ok := any(in).(urlArgs); ok {
		var id string
		id, query = a.args()
		path = strings.Replace(path, "{id}", url.PathEscape(id), 1)
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var body io.Reader
	if rt.method == http.MethodPost {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, rt.method, "http://agent"+path, body)
	if err != nil {
		return nil, err
	}
	// net/http sends a request again on a new stream when the stream
	// reports that it wrote nothing, if it can read the body again. A yamux
	// stream can report that after the agent got the request, so the body
	// cannot be read again, and a POST never goes two times.
	req.GetBody = nil
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (rt route[In]) read(w http.ResponseWriter, r *http.Request) (In, error) {
	var in In
	if rt.method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
			return in, fmt.Errorf("bad request body: %w", err)
		}
	}
	if p, ok := any(&in).(urlParser); ok {
		if err := p.parse(r.PathValue("id"), r.URL.Query()); err != nil {
			return in, err
		}
	}
	return in, nil
}

func call[In, Out any](ctx context.Context, s *Session, o op[In, Out], in In) (Out, error) {
	var out Out
	resp, err := open(ctx, s, o.route, in)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if _, ok := any(out).(none); ok {
		return out, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("%s %s on server %s: read the answer: %w", o.method, o.path, s.ServerID(), s.closedOr(err))
	}
	return out, nil
}

func open[In any](ctx context.Context, s *Session, rt route[In], in In) (*http.Response, error) {
	req, err := rt.request(ctx, in)
	if err != nil {
		return nil, err
	}
	return s.do(req)
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

func serveLogs(w http.ResponseWriter, r *http.Request, a *agent, req logsRequest) {
	rc, err := a.Node.Driver().Logs(r.Context(), req.ID, req.Follow)
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

// serveShell upgrades the stream to a shell. See readFrames for the stream
// format.
func serveShell(w http.ResponseWriter, r *http.Request, a *agent, req shellRequest) {
	if r.Header.Get("Upgrade") != shellProtocol {
		http.Error(w, "the shell needs Upgrade: "+shellProtocol, http.StatusBadRequest)
		return
	}
	term, err := a.Node.Driver().Shell(r.Context(), req.ID, req.Size)
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

// serveEnsureImage streams the progress as JSON lines. The last line has
// the image or the error.
func serveEnsureImage(w http.ResponseWriter, r *http.Request, a *agent, ref sandbox.ImageRef) {
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
	img, err := a.Node.EnsureImage(r.Context(), ref.Ref, func(p image.Progress) {
		wp := wireProgress(p)
		send(imageMessage{Progress: &wp})
	})
	if err != nil {
		send(imageMessage{Error: err.Error()})
		return
	}
	send(imageMessage{Image: &img})
}
