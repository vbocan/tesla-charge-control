package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Who may reach the dashboard.
//
//   - A visitor on the LAN, connecting directly, is trusted as before.
//   - A request that arrives through a proxy (a Cloudflare Tunnel, Caddy) is never
//     trusted for its address: the proxy runs on the Pi, so every such request
//     comes from 127.0.0.1 whoever the visitor is. If Cloudflare Access is
//     configured it must carry a valid Access token (verified in access.go);
//     otherwise it needs the access token (auth_token).
//   - Any other request needs the access token.
//
// Failed attempts at the access token are rate limited, so a short token cannot
// be guessed by brute force.

type ctxKey int

const ctxWho ctxKey = iota

// proxyHeaders: if a request carries any of them, it came through a proxy.
// A LAN client can add them too, but that only moves it into the stricter group.
var proxyHeaders = []string{
	"Cf-Connecting-Ip", "Cf-Ray", "Cf-Access-Jwt-Assertion",
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded",
}

// isProxied reports whether the request arrived through a reverse proxy.
func isProxied(r *http.Request) bool {
	for _, h := range proxyHeaders {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// clientIP is the visitor's address as best it can be told. Behind a proxy it
// comes from the proxy's headers, which a direct caller can forge, so it is used
// only for rate-limit buckets and log lines, never to grant access.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// who describes the authenticated caller for audit log lines.
func who(r *http.Request) string {
	if v, ok := r.Context().Value(ctxWho).(string); ok {
		return v
	}
	return "client " + clientIP(r)
}

func withWho(r *http.Request, who string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxWho, who))
}

// failLimiter counts failed attempts per key within a sliding window.
type failLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	fails  map[string][]time.Time
}

func newFailLimiter(max int, window time.Duration) *failLimiter {
	return &failLimiter{max: max, window: window, now: time.Now, fails: map[string][]time.Time{}}
}

// recent returns the failures of key still inside the window. Caller holds mu.
func (l *failLimiter) recent(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	kept := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = kept
	return kept
}

// blocked reports whether key has used up its failed attempts.
func (l *failLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key)) >= l.max
}

// fail records one failed attempt by key.
func (l *failLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > 1024 { // keep the map bounded under a flood of distinct keys
		for k := range l.fails {
			l.recent(k)
		}
	}
	l.fails[key] = append(l.recent(key), l.now())
}

// authEnabled reports whether the auth middleware applies. It does whenever a
// token is set, Access is configured, or a tunnel is in use — in which case an
// unset token must not leave the dashboard open to the internet.
func (s *server) authEnabled() bool {
	return s.cfg.AuthToken != "" || s.accessRequired || s.cfg.CloudflareTunnel
}

// initAuth sets up the auth state from the config and says what it found.
func (s *server) initAuth() {
	s.limiter = newFailLimiter(10, time.Minute)

	team := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s.cfg.AccessTeamDomain), "https://"), "/")
	aud := strings.TrimSpace(s.cfg.AccessAUD)
	s.accessRequired = team != "" || aud != ""
	switch {
	case s.accessRequired && (team == "" || aud == ""):
		log.Printf("ACCESS MISCONFIGURED: access_team_domain and access_aud must both be set; refusing every proxied request until they are")
	case s.accessRequired:
		s.access = newAccessVerifier(team, aud)
		log.Printf("Cloudflare Access: requests through a proxy must carry a valid Access token for %s", team)
	}
	if s.cfg.CloudflareTunnel && !s.accessRequired {
		log.Printf("WARNING: cloudflare_tunnel is on but Cloudflare Access is not configured; proxied requests need auth_token")
	}
	if s.cfg.CloudflareTunnel && s.cfg.AuthToken == "" && !s.accessRequired {
		log.Printf("WARNING: no auth_token and no Access: every proxied request will be refused")
	}
}

// verifyAccess checks the Cloudflare Access token on a proxied request.
func (s *server) verifyAccess(r *http.Request) (string, error) {
	if s.access == nil {
		return "", errors.New("access is misconfigured")
	}
	return s.access.Verify(accessToken(r))
}

// tokenOK reports whether the request carries the valid access-token cookie.
func (s *server) tokenOK(r *http.Request) bool {
	if s.cfg.AuthToken == "" {
		return false
	}
	c, err := r.Cookie("auth")
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.cfg.AuthToken)) == 1
}

// authMiddleware decides who may reach the dashboard (see the top of this file).
// With the access token: /?token=SECRET sets a cookie and redirects to /.
func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied := isProxied(r)

		// Through a proxy the socket address says nothing about the visitor, so it
		// never earns the LAN shortcut below.
		if proxied && s.accessRequired {
			email, err := s.verifyAccess(r)
			if err != nil {
				log.Printf("[ACCESS] refused %s %s from %s: %v", r.Method, r.URL.Path, clientIP(r), err)
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, withWho(r, email+" (Cloudflare Access)"))
			return
		}

		// Allow LAN access without auth, when the client really is on the LAN.
		if !proxied && isPrivateAddr(r.RemoteAddr) {
			next.ServeHTTP(w, withWho(r, "LAN client "+clientIP(r)))
			return
		}

		// Everything else needs the access token.
		if s.cfg.AuthToken == "" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		ip := clientIP(r)
		if s.limiter.blocked(ip) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many failed attempts", http.StatusTooManyRequests)
			return
		}

		// Check query param — if present, set cookie and redirect to clean URL
		if tok := r.URL.Query().Get("token"); tok != "" {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AuthToken)) == 1 {
				http.SetCookie(w, &http.Cookie{
					Name:     "auth",
					Value:    s.cfg.AuthToken,
					Path:     "/",
					MaxAge:   365 * 24 * 3600, // 1 year
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
				})
				// Redirect to the same path without the token param
				q := r.URL.Query()
				q.Del("token")
				r.URL.RawQuery = q.Encode()
				http.Redirect(w, r, r.URL.String(), http.StatusFound)
				return
			}
			s.limiter.fail(ip)
		}

		// Check cookie
		if s.tokenOK(r) {
			next.ServeHTTP(w, withWho(r, "token holder "+ip))
			return
		}
		if _, err := r.Cookie("auth"); err == nil {
			s.limiter.fail(ip) // a wrong cookie is a failed guess; no cookie at all is just a first visit
		}

		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}
