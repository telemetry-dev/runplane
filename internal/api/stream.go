package api

import (
	"io"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// Response documentation for the operations that do not send JSON.

func textResponse(op *huma.Operation) {
	op.Responses = map[string]*huma.Response{"200": {
		Description: "The log as plain text. With follow=1, the response streams until the log ends.",
		Content:     map[string]*huma.MediaType{"text/plain": {Schema: &huma.Schema{Type: huma.TypeString}}},
	}}
}

func binaryResponse(contentType string) func(*huma.Operation) {
	return func(op *huma.Operation) {
		op.Responses = map[string]*huma.Response{"200": {
			Description: "OK",
			Content:     map[string]*huma.MediaType{contentType: {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}},
		}}
	}
}

func websocketResponse(op *huma.Operation) {
	op.DefaultStatus = http.StatusSwitchingProtocols
	op.Responses = map[string]*huma.Response{"101": {
		Description: "The connection becomes a WebSocket. Binary frames carry the terminal bytes in both directions. " +
			`The client resizes the terminal with a text frame {"type":"resize","rows":N,"cols":N}.`,
	}}
}

// stream sends rc to the client. With follow, it flushes after each read,
// and it ends when the client leaves or the server shuts down.
func (h *Handler) stream(rc io.ReadCloser, contentType string, follow bool) *huma.StreamResponse {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		_, w := humago.Unwrap(hctx)
		var once sync.Once
		closeReader := func() { once.Do(func() { rc.Close() }) }
		defer closeReader()

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		if !follow {
			w.WriteHeader(http.StatusOK)
			io.Copy(w, rc)
			return
		}
		// A reverse proxy must not buffer the stream.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		rcw := http.NewResponseController(w)
		rcw.Flush()

		finished := make(chan struct{})
		defer close(finished)
		go func() {
			select {
			case <-h.done:
			case <-hctx.Context().Done():
			case <-finished:
			}
			closeReader()
		}()
		buf := make([]byte, 32<<10)
		for {
			n, err := rc.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				rcw.Flush()
			}
			if err != nil {
				return
			}
		}
	}}
}
