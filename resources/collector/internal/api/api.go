// Package api exposes the collector over HTTP: the ingest endpoint agents
// report to, and read endpoints the display layer consumes.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"vpsm-collector/internal/config"
	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/query"
)

// MaxBodyBytes caps the request body as it arrives on the wire. A full
// batch of protocol.MaxSamplesPerReport samples carrying per-process data
// runs to a few megabytes; anything past this is a bug or an attack, and
// either way should not be buffered.
const MaxBodyBytes = 16 << 20

// MaxDecodedBytes caps a gzipped body after decompression, so a small
// request cannot expand without limit. Compression is optional for agents
// (see protocol); this is what makes accepting it safe.
const MaxDecodedBytes = 64 << 20

// MaxClockSkew is how far into the future a sample may be stamped before
// the collector rejects it. An agent with a broken clock would otherwise
// pin itself to the top of every "latest" query forever. Timestamps in the
// past are accepted without limit: that is legitimate backfill, and
// retention handles anything genuinely ancient.
const MaxClockSkew = 24 * time.Hour

// MaxHistory caps how many samples a read request may ask for per host.
const MaxHistory = 1000

// Ingester stores reports. Satisfied by *storage.Store.
type Ingester interface {
	SaveReport(ctx context.Context, r *protocol.Report, receivedAt time.Time) (protocol.Response, error)
}

// Options configures a Server.
type Options struct {
	Store  Ingester
	Source query.Source
	Logger *slog.Logger

	// Token is the bearer token required on every endpoint but /healthz.
	// Empty means unauthenticated, which config.Validate only permits when
	// the operator asked for it explicitly.
	Token string

	// MaxClockSkew overrides how far into the future a sample may be
	// stamped before it is rejected. Zero uses MaxClockSkew.
	MaxClockSkew time.Duration

	// Now is overridable for tests.
	Now func() time.Time
}

// Server routes the collector's HTTP endpoints.
type Server struct {
	store   Ingester
	source  query.Source
	log     *slog.Logger
	token   string
	maxSkew time.Duration
	now     func() time.Time
}

// Handler builds the collector's HTTP handler.
func Handler(o Options) http.Handler {
	s := &Server{
		store:   o.Store,
		source:  o.Source,
		log:     o.Logger,
		token:   o.Token,
		maxSkew: o.MaxClockSkew,
		now:     o.Now,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.maxSkew <= 0 {
		s.maxSkew = MaxClockSkew
	}

	mux := http.NewServeMux()
	// Method is part of the pattern, so a GET to the ingest endpoint gets a
	// 405 from the mux rather than falling through to a handler.
	mux.HandleFunc("POST "+protocol.ReportsPath, s.authed(s.handleReports))
	mux.HandleFunc("GET /"+protocol.Version+"/hosts", s.authed(s.handleHosts))
	mux.HandleFunc("GET /"+protocol.Version+"/hosts/{id}", s.authed(s.handleHost))

	// Unauthenticated on purpose: it reveals nothing and needs to work for
	// load balancer and systemd health checks.
	mux.HandleFunc("GET /healthz", s.handleHealth)

	return s.logRequests(mux)
}

// authed requires a bearer token when one is configured.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			next(w, r)
			return
		}
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if len(got) <= len(prefix) || got[:len(prefix)] != prefix ||
			subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vpsm"`)
			s.fail(w, r, http.StatusUnauthorized, protocol.CodeUnauthorized, "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
}

// logRequests records one line per request, including the status so a
// misconfigured agent's 401s are visible in the collector's log.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		s.log.LogAttrs(r.Context(), level, "http",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("took", s.now().Sub(start)),
			slog.String("remote", r.RemoteAddr),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status, r.wroteHeader = status, true
	r.ResponseWriter.WriteHeader(status)
}

// fail writes a non-2xx response carrying both a message for the log and a
// stable code for the agent to branch on.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code protocol.ErrorCode, msg string) {
	s.write(w, r, status, protocol.Error{Error: msg, Code: code})
}

func (s *Server) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already out, so this can only be logged.
		s.log.WarnContext(r.Context(), "write response", slog.Any("err", err))
	}
}

// history reads the ?history= parameter, which controls how many samples per
// host a read returns.
func history(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("history")
	if raw == "" {
		return config.DefaultHistory, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if n < 1 {
		n = 1
	}
	return min(n, MaxHistory), nil
}
