// Package middleware contains HTTP middleware: request id, recovery, logging, auth, permission, rate limit.
package middleware

import (
	"context"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/auth/token"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/platform/reqmeta"
	"github.com/ritel35k/backend/pkg/apperr"
)

func RequestID(trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := r.Header.Get("X-Request-ID")
			if rid == "" || len(rid) > 100 {
				rid = uuid.NewString()
			}
			w.Header().Set("X-Request-ID", rid)
			m := reqmeta.Meta{RequestID: rid, IP: clientIP(r, trustProxy), UserAgent: r.UserAgent(), DeviceID: r.Header.Get("X-Device-ID")}
			next.ServeHTTP(w, r.WithContext(reqmeta.With(r.Context(), m)))
		})
	}
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func Recoverer(log *zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error().Interface("panic", rec).Bytes("stack", debug.Stack()).Msg("panic recovered")
					response.Error(w, r, log, apperr.Internal(nil))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }

// Unwrap lets http.ResponseController reach the real writer (flush, per-request deadlines for streams).
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func AccessLog(log *zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			log.Info().Str("request_id", reqmeta.From(r.Context()).RequestID).Str("method", r.Method).
				Str("path", r.URL.Path).Int("status", sw.status).Dur("dur", time.Since(start)).Msg("http")
		})
	}
}

func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// PrincipalLoader resolves a verified token subject into a Principal with grants.
type PrincipalLoader interface {
	LoadPrincipal(ctx context.Context, userID, companyID uuid.UUID) (*access.Principal, error)
}

// Guard builds authentication and permission middleware for module routes.
type Guard struct {
	Tokens *token.Manager
	Loader PrincipalLoader
	Log    *zerolog.Logger
}

func (g *Guard) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		raw, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || raw == "" {
			response.Error(w, r, g.Log, apperr.Unauthenticated("missing bearer token"))
			return
		}
		uid, cid, err := g.Tokens.ParseAccess(raw)
		if err != nil {
			response.Error(w, r, g.Log, apperr.Unauthenticated("invalid or expired token"))
			return
		}
		p, err := g.Loader.LoadPrincipal(r.Context(), uid, cid)
		if err != nil {
			response.Error(w, r, g.Log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(access.WithPrincipal(r.Context(), p)))
	})
}

// Perm requires the caller to hold perm in at least one scope. Resource-level checks
// (is *this* store inside the scope?) are done by the service with Grants.Can / ScopeFilter.
func (g *Guard) Perm(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := access.MustPrincipal(r.Context())
			if !p.Grants.Has(perm) {
				response.Error(w, r, g.Log, apperr.Forbidden("missing permission").WithDetails(map[string]string{"permission": perm}))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AnyPerm requires at least one of the permissions.
func (g *Guard) AnyPerm(perms ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := access.MustPrincipal(r.Context())
			for _, perm := range perms {
				if p.Grants.Has(perm) {
					next.ServeHTTP(w, r)
					return
				}
			}
			response.Error(w, r, g.Log, apperr.Forbidden("missing permission").WithDetails(map[string]any{"any_of": perms}))
		})
	}
}

// IPRateLimit is a per-IP token bucket kept in memory (swap for Redis when running >1 API instance).
type IPRateLimit struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
	trust   bool
	log     *zerolog.Logger
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewIPRateLimit(perMinute, burst int, trustProxy bool, log *zerolog.Logger) *IPRateLimit {
	return &IPRateLimit{buckets: map[string]*bucket{}, rate: float64(perMinute) / 60, burst: float64(burst), trust: trustProxy, log: log}
}

func (l *IPRateLimit) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, l.trust)
		now := time.Now()
		l.mu.Lock()
		b := l.buckets[ip]
		if b == nil {
			b = &bucket{tokens: l.burst, last: now}
			l.buckets[ip] = b
		}
		b.tokens = minf(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
		b.last = now
		allowed := b.tokens >= 1
		if allowed {
			b.tokens--
		}
		if len(l.buckets) > 10000 { // opportunistic cleanup
			for k, v := range l.buckets {
				if now.Sub(v.last) > 10*time.Minute {
					delete(l.buckets, k)
				}
			}
		}
		l.mu.Unlock()
		if !allowed {
			w.Header().Set("Retry-After", "30")
			response.Error(w, r, l.log, apperr.New(http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
