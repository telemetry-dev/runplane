package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	sessionCookie = "runplane_session"
	deviceCookie  = "runplane_device"
	csrfHeader    = "X-Runplane-CSRF"
)

// client is what handlers need to know about the caller.
type client struct {
	ip        string // for the rate limiter and the audit log
	userAgent string
	https     bool
	host      string // Host header, for URLs when public_url is empty
	token     string // session token from the cookie
	device    string // device token from the cookie (auth.Service.Device)
}

type clientKey struct{}

func clientOf(ctx context.Context) client {
	c, _ := ctx.Value(clientKey{}).(client)
	return c
}

// baseURL is the browser URL of runplane without a trailing slash: the
// public_url setting, or the scheme and host of the request.
func (h *Handler) baseURL(ctx context.Context) (string, error) {
	st, err := h.d.Store.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	if st.PublicURL != "" {
		return strings.TrimSuffix(st.PublicURL, "/"), nil
	}
	c := clientOf(ctx)
	scheme := "http"
	if c.https {
		scheme = "https"
	}
	return scheme + "://" + c.host, nil
}

// guard sets the security headers, checks the CSRF header, and puts the
// client into the request context.
func (h *Handler) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "same-origin")
		hdr.Set("X-Frame-Options", "DENY")
		if h.d.Config.Dev && devCORS(w, r) {
			return
		}
		// A form or script on another site cannot set this header, so it
		// cannot use the session cookie to change anything.
		if strings.HasPrefix(r.URL.Path, "/api/") && !safeMethod(r.Method) && r.Header.Get(csrfHeader) != "1" {
			writeProblem(w, http.StatusForbidden, "The request needs the header "+csrfHeader+": 1.")
			return
		}
		c := client{ip: clientIP(r), userAgent: r.UserAgent(), https: isHTTPS(r), host: r.Host}
		if ck, err := r.Cookie(sessionCookie); err == nil {
			c.token = ck.Value
		}
		if ck, err := r.Cookie(deviceCookie); err == nil {
			c.device = ck.Value
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, c)))
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// devCORS lets the Vite dev server on another localhost port call the API.
// It reports whether it answered a preflight request.
func devCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	u, err := url.Parse(origin)
	if origin == "" || err != nil || !isLoopbackHost(u.Hostname()) {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Add("Vary", "Origin")
	if r.Method != http.MethodOptions {
		h.Set("Access-Control-Expose-Headers", stepFilterHeader)
		return false
	}
	h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
	h.Set("Access-Control-Allow-Headers", "Content-Type, "+csrfHeader)
	w.WriteHeader(http.StatusNoContent)
	return true
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// clientIP is the address of the peer. Behind a reverse proxy on the same
// host, it is the last address in X-Forwarded-For, which the proxy added.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isLoopbackHost(host) {
		return host
	}
	xff := r.Header.Values("X-Forwarded-For")
	if len(xff) == 0 {
		return host
	}
	parts := strings.Split(xff[len(xff)-1], ",")
	if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
		return ip
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// logRequests logs changes and failures at info level, and the other
// requests at debug level. It logs the path without the query, because the
// query can hold a setup code.
func (h *Handler) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		level := slog.LevelDebug
		if rec.status >= 400 || !safeMethod(r.Method) {
			level = slog.LevelInfo
		}
		h.d.Log.Log(r.Context(), level, "http request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start).Round(time.Millisecond), "ip", clientIP(r))
	})
}

// recorder keeps the status code. Unwrap gives http.ResponseController,
// the SSE writer, and the WebSocket upgrade the flush, deadline, and hijack
// methods of the original writer.
type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// writeProblem writes an RFC 9457 problem outside of Huma.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}
