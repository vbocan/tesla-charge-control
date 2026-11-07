package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vbocan/tesla-charge-control/deye"
)

const testToken = "s3cret-token"

// authRig is a server with the auth middleware around a handler that reports
// who it thinks the caller is.
type authRig struct {
	s       *server
	handler http.Handler
	rig     *accessRig
}

// newAuthRig builds a server from cfg. If cfg configures Access, the verifier is
// pointed at a fake Cloudflare key endpoint.
func newAuthRig(t *testing.T, cfg deye.Config) *authRig {
	t.Helper()
	s := &server{cfg: cfg}
	s.initAuth()
	rig := newAccessRig(t)
	if s.access != nil {
		s.access = rig.v
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok: " + who(r)))
	})
	return &authRig{s: s, handler: s.authMiddleware(ok), rig: rig}
}

func accessConfig() deye.Config {
	return deye.Config{AuthToken: testToken, AccessTeamDomain: testTeam, AccessAUD: testAUD, CloudflareTunnel: true}
}

// do sends a request with the given socket address and headers.
func (a *authRig) do(remote string, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)
	return w
}

func (a *authRig) accessJWT(t *testing.T) string { return a.rig.token(t, nil) }

const (
	lanAddr    = "192.168.1.20:51234" // a LAN client connecting directly
	tunnelAddr = "127.0.0.1:51234"    // cloudflared (or Caddy) on the Pi itself
	publicAddr = "203.0.113.9:51234"  // an internet client connecting directly
)

var viaTunnel = map[string]string{"Cf-Connecting-Ip": "198.51.100.7"}

func authCookie() *http.Cookie { return &http.Cookie{Name: "auth", Value: testToken} }

func TestLANClientsConnectingDirectlyAreTrusted(t *testing.T) {
	a := newAuthRig(t, accessConfig())
	w := a.do(lanAddr, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "LAN client 192.168.1.20") {
		t.Errorf("%d %q, want 200 for a LAN client", w.Code, w.Body.String())
	}
}

// The bug this exists to prevent: cloudflared runs on the Pi, so tunnelled
// requests come from 127.0.0.1. That must not earn the LAN shortcut.
func TestTunnelledRequestsWithoutAnAccessTokenAreRefused(t *testing.T) {
	a := newAuthRig(t, accessConfig())
	if w := a.do(tunnelAddr, viaTunnel); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403 for a tunnelled request with no Access token", w.Code)
	}
}

func TestTunnelledRequestsWithAValidAccessTokenAreAdmitted(t *testing.T) {
	a := newAuthRig(t, accessConfig())
	h := map[string]string{"Cf-Connecting-Ip": "198.51.100.7", "Cf-Access-Jwt-Assertion": a.accessJWT(t)}
	w := a.do(tunnelAddr, h)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "me@example.com (Cloudflare Access)") {
		t.Errorf("%d %q, want 200 as the Access identity", w.Code, w.Body.String())
	}
}

func TestAccessTokenInTheCookieIsAcceptedToo(t *testing.T) {
	a := newAuthRig(t, accessConfig())
	w := a.do(tunnelAddr, viaTunnel, &http.Cookie{Name: "CF_Authorization", Value: a.accessJWT(t)})
	if w.Code != 200 {
		t.Errorf("status %d, want 200", w.Code)
	}
}

// With Access configured the short access token is no longer a way in through
// the tunnel: it is the guessable credential.
func TestTheAccessTokenDoesNotOpenTheTunnelWhenAccessIsConfigured(t *testing.T) {
	a := newAuthRig(t, accessConfig())
	if w := a.do(tunnelAddr, viaTunnel, authCookie()); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", w.Code)
	}
}

// A LAN client can add proxy headers, but that only moves it into the stricter group.
func TestSpoofedProxyHeadersFromTheLANNeedAnAccessToken(t *testing.T) {
	a := newAuthRig(t, accessConfig())
	if w := a.do(lanAddr, viaTunnel); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", w.Code)
	}
}

func TestAccessTokenForTheWrongApplicationIsRefused(t *testing.T) {
	a := newAuthRig(t, accessConfig())
	wrong := a.rig.token(t, func(c map[string]any) { c["aud"] = []string{"another-application"} })
	h := map[string]string{"Cf-Connecting-Ip": "198.51.100.7", "Cf-Access-Jwt-Assertion": wrong}
	if w := a.do(tunnelAddr, h); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", w.Code)
	}
}

// Half-configured Access must fail closed, not fall back to something weaker.
func TestHalfConfiguredAccessRefusesEverythingProxied(t *testing.T) {
	cfg := accessConfig()
	cfg.AccessAUD = ""
	a := newAuthRig(t, cfg)
	if a.s.access != nil {
		t.Fatal("a verifier was built from half a configuration")
	}
	if w := a.do(tunnelAddr, viaTunnel, authCookie()); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403 even with the access token", w.Code)
	}
	if w := a.do(lanAddr, nil); w.Code != 200 {
		t.Errorf("a direct LAN client got %d, want 200", w.Code)
	}
}

// The old hole, without Access: a proxy on the Pi (Caddy) made every visitor
// look local. Proxied requests now need the access token.
func TestProxiedRequestsNeedTheTokenWhenThereIsNoAccess(t *testing.T) {
	a := newAuthRig(t, deye.Config{AuthToken: testToken})
	caddy := map[string]string{"X-Forwarded-For": "198.51.100.7"}

	if w := a.do(tunnelAddr, caddy); w.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401 for a proxied request with no token", w.Code)
	}
	if w := a.do(tunnelAddr, caddy, authCookie()); w.Code != 200 || !strings.Contains(w.Body.String(), "token holder 198.51.100.7") {
		t.Errorf("%d %q, want 200 as a token holder", w.Code, w.Body.String())
	}
}

func TestDirectInternetClientsNeedTheToken(t *testing.T) {
	a := newAuthRig(t, deye.Config{AuthToken: testToken})
	if w := a.do(publicAddr, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", w.Code)
	}
	if w := a.do(publicAddr, nil, authCookie()); w.Code != 200 {
		t.Errorf("status %d, want 200 with the cookie", w.Code)
	}
}

func TestTokenInTheQuerySetsACookie(t *testing.T) {
	a := newAuthRig(t, deye.Config{AuthToken: testToken})
	r := httptest.NewRequest("GET", "/?token="+testToken+"&x=1", nil)
	r.RemoteAddr = publicAddr
	w := httptest.NewRecorder()
	a.handler.ServeHTTP(w, r)

	if w.Code != http.StatusFound || w.Header().Get("Location") != "/?x=1" {
		t.Fatalf("%d -> %q, want a redirect to the URL without the token", w.Code, w.Header().Get("Location"))
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].Name != "auth" || !c[0].HttpOnly {
		t.Errorf("cookies %v, want one HttpOnly auth cookie", c)
	}
}

// Without Access the tunnel is only safe if something still guards it: a tunnel
// with no token and no Access must refuse proxied requests, not open them.
func TestTunnelWithNoTokenAndNoAccessRefusesProxiedRequests(t *testing.T) {
	a := newAuthRig(t, deye.Config{CloudflareTunnel: true})
	if !a.s.authEnabled() {
		t.Fatal("auth must be on whenever the tunnel is")
	}
	if w := a.do(tunnelAddr, viaTunnel); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", w.Code)
	}
	if w := a.do(lanAddr, nil); w.Code != 200 {
		t.Errorf("a direct LAN client got %d, want 200", w.Code)
	}
}

func TestInternetClientsAreRefusedWhenAccessIsTheOnlyCredential(t *testing.T) {
	cfg := accessConfig()
	cfg.AuthToken = ""
	a := newAuthRig(t, cfg)
	if w := a.do(publicAddr, nil); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403: there is no token to present", w.Code)
	}
}

func TestAuthIsOffWhenNothingIsConfigured(t *testing.T) {
	a := newAuthRig(t, deye.Config{})
	if a.s.authEnabled() {
		t.Error("auth should be disabled with no token, no Access and no tunnel (the old default)")
	}
}

// A short token must not be guessable by brute force.
func TestFailedTokenAttemptsAreRateLimited(t *testing.T) {
	a := newAuthRig(t, deye.Config{AuthToken: testToken})
	wrong := &http.Cookie{Name: "auth", Value: "000000"}

	for i := 0; i < 10; i++ {
		if w := a.do(publicAddr, nil, wrong); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, w.Code)
		}
	}
	// Locked out: even the right token is refused for now.
	if w := a.do(publicAddr, nil, authCookie()); w.Code != http.StatusTooManyRequests {
		t.Errorf("status %d after 10 failures, want 429", w.Code)
	}
	// Another client is unaffected.
	if w := a.do("203.0.113.77:1", nil, authCookie()); w.Code != 200 {
		t.Errorf("a different client got %d, want 200", w.Code)
	}
}

func TestVisitingWithoutAnyCredentialIsNotCountedAsAnAttack(t *testing.T) {
	a := newAuthRig(t, deye.Config{AuthToken: testToken})
	for i := 0; i < 30; i++ {
		a.do(publicAddr, nil)
	}
	if w := a.do(publicAddr, nil, authCookie()); w.Code != 200 {
		t.Errorf("status %d, a first visit repeated must not lock anyone out", w.Code)
	}
}

func TestFailLimiterWindowAndBound(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := newFailLimiter(3, time.Minute)
	l.now = clk.Now

	l.fail("a")
	l.fail("a")
	if l.blocked("a") {
		t.Error("blocked after 2 of 3 allowed failures")
	}
	l.fail("a")
	if !l.blocked("a") {
		t.Error("not blocked after 3 failures")
	}
	clk.Advance(61 * time.Second)
	if l.blocked("a") {
		t.Error("still blocked after the window passed")
	}

	for i := 0; i < 3000; i++ { // a flood of distinct addresses must not grow without bound
		l.fail(string(rune('a'+i%26)) + time.Duration(i).String())
		clk.Advance(time.Second)
	}
	l.mu.Lock()
	n := len(l.fails)
	l.mu.Unlock()
	if n > 1100 {
		t.Errorf("%d tracked clients, want the map kept bounded", n)
	}
}

func TestIsProxiedAndClientIP(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		headers map[string]string
		proxied bool
		ip      string
	}{
		{"direct LAN", lanAddr, nil, false, "192.168.1.20"},
		{"Cloudflare", tunnelAddr, map[string]string{"Cf-Connecting-Ip": "198.51.100.7"}, true, "198.51.100.7"},
		{"Cloudflare Access header only", tunnelAddr, map[string]string{"Cf-Access-Jwt-Assertion": "x"}, true, "127.0.0.1"},
		{"Caddy", tunnelAddr, map[string]string{"X-Forwarded-For": "198.51.100.7, 10.0.0.1"}, true, "198.51.100.7"},
		{"Cloudflare wins over X-Forwarded-For", tunnelAddr,
			map[string]string{"Cf-Connecting-Ip": "198.51.100.7", "X-Forwarded-For": "1.1.1.1"}, true, "198.51.100.7"},
		{"RFC 7239", tunnelAddr, map[string]string{"Forwarded": "for=198.51.100.7"}, true, "127.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remote
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := isProxied(r); got != tc.proxied {
				t.Errorf("isProxied = %v, want %v", got, tc.proxied)
			}
			if got := clientIP(r); got != tc.ip {
				t.Errorf("clientIP = %q, want %q", got, tc.ip)
			}
		})
	}
}

func TestAccessTeamDomainIsNormalised(t *testing.T) {
	s := &server{cfg: deye.Config{AccessTeamDomain: "https://" + testTeam + "/", AccessAUD: testAUD}}
	s.initAuth()
	if s.access == nil {
		t.Fatal("no verifier was built")
	}
	if s.access.team != testTeam {
		t.Errorf("team %q, want %q (scheme and trailing slash stripped)", s.access.team, testTeam)
	}
}
