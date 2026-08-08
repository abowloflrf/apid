package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/abowloflrf/apid/stats"
)

type accessLogState struct {
	route    string
	forward  *stats.Record
	panicked bool
}

type accessLogStateKey struct{}

func accessLogFromContext(ctx context.Context) *accessLogState {
	state, _ := ctx.Value(accessLogStateKey{}).(*accessLogState)
	return state
}

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &respRecorder{ResponseWriter: w}
		state := &accessLogState{}
		r = r.WithContext(context.WithValue(r.Context(), accessLogStateKey{}, state))

		defer func() {
			if recovered := recover(); recovered != nil {
				state.panicked = true
				if !rec.wroteFinalHeader() {
					rec.status = http.StatusInternalServerError
				}
				s.logAccess(r, rec, state, time.Since(start))
				panic(recovered)
			}
			s.logAccess(r, rec, state, time.Since(start))
		}()

		next.ServeHTTP(rec, r)
	})
}

func (s *Server) logAccess(r *http.Request, rec *respRecorder, state *accessLogState, duration time.Duration) {
	status := rec.statusCode()
	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"client_ip", remoteIP(r.RemoteAddr),
		"user_agent", r.UserAgent(),
		"status", status,
		"response_bytes", rec.bytes,
		"duration", duration.Round(time.Millisecond),
	}
	if state.route != "" {
		attrs = append(attrs, "route", state.route)
	}
	if stat := state.forward; stat != nil {
		attrs = append(attrs,
			"client_protocol", stat.ClientProtocol,
			"model", stat.ClientModel,
			"stream", stat.Stream,
			"upstream_protocol", stat.UpstreamProtocol,
			"upstream_url", urlWithoutQuery(stat.UpstreamURL),
			"upstream_model", stat.UpstreamModel,
			"upstream_status", stat.UpstreamStatus,
		)
		if usage := stat.Usage; usage != nil {
			attrs = append(attrs, slog.Group("usage",
				"input", usage.InputTokens,
				"output", usage.OutputTokens,
				"total", usage.TotalTokens,
				"cache_read", usage.CachedTokens,
				"cache_creation", usage.CacheCreationTokens,
			))
		}
	}
	if state.panicked {
		attrs = append(attrs, "panic", true)
	}

	s.log.Log(r.Context(), accessLogLevel(status), "access", attrs...)
}

func accessLogLevel(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func remoteIP(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	if ip := net.ParseIP(strings.Trim(remoteAddr, "[]")); ip != nil {
		return ip.String()
	}
	return remoteAddr
}

func urlWithoutQuery(rawURL string) string {
	base, _, _ := strings.Cut(rawURL, "?")
	return base
}

type respRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *respRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if r.status != 0 {
		return
	}
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *respRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *respRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *respRecorder) wroteFinalHeader() bool { return r.status != 0 }

func (r *respRecorder) Flush() {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *respRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
