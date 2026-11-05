package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// holdGrace is how long a debug sandbox waits for a hold after the runner
// exits. The host sets the hold only after it sees the job complete.
const holdGrace = 2 * time.Minute

// hostRequests keeps what the host asks for: a hold time and a shutdown.
type hostRequests struct {
	mu      sync.Mutex
	hold    time.Time
	changed chan struct{} // closed and replaced when the hold changes

	stop     chan struct{}
	stopOnce sync.Once
}

func newHostRequests() *hostRequests {
	return &hostRequests{changed: make(chan struct{}), stop: make(chan struct{})}
}

func (h *hostRequests) setHold(until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hold = until
	close(h.changed)
	h.changed = make(chan struct{})
}

func (h *hostRequests) shutdown() {
	h.stopOnce.Do(func() { close(h.stop) })
}

// stopped is closed after a shutdown request.
func (h *hostRequests) stopped() <-chan struct{} {
	return h.stop
}

func (h *hostRequests) shuttingDown() bool {
	select {
	case <-h.stop:
		return true
	default:
		return false
	}
}

// waitHold returns at the hold time or at a shutdown. The host can move the
// hold while init waits. With debug and no hold, it waits grace for one.
func (h *hostRequests) waitHold(debug bool, grace time.Duration) {
	var graceEnd time.Time
	if debug {
		graceEnd = time.Now().Add(grace)
	}
	for {
		h.mu.Lock()
		end, changed := h.hold, h.changed
		h.mu.Unlock()
		if end.IsZero() {
			end = graceEnd
		}
		wait := time.Until(end)
		if wait <= 0 {
			return
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-changed:
		case <-h.stop:
			timer.Stop()
			return
		}
		timer.Stop()
	}
}

// guestMux serves the requests that the host sends to the guest.
func guestMux(req *hostRequests, shells *shellServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/hold", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Until time.Time `json:"until"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad hold: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.setHold(body.Until)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		req.shutdown()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/shell", shells.handleOpen)
	mux.HandleFunc("POST /v1/shell/resize", shells.handleResize)
	return mux
}
