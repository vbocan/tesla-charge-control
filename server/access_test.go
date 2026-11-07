package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testTeam = "team.cloudflareaccess.com"
	testAUD  = "aud-1234567890"
)

var (
	testKeyOnce  sync.Once
	testKeyA     *rsa.PrivateKey // the key Cloudflare "publishes"
	testKeyOther *rsa.PrivateKey // an attacker's key
)

func testKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	testKeyOnce.Do(func() {
		var err error
		if testKeyA, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if testKeyOther, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return testKeyA, testKeyOther
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwks serves Cloudflare's key set for the given keys (kid -> public key) and
// counts how many times it is fetched.
func jwks(t *testing.T, keys map[string]*rsa.PublicKey) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var set struct {
			Keys []map[string]string `json:"keys"`
		}
		for kid, pub := range keys {
			e := big.NewInt(int64(pub.E)).Bytes()
			set.Keys = append(set.Keys, map[string]string{
				"kid": kid, "kty": "RSA", "alg": "RS256", "use": "sig",
				"n": b64(pub.N.Bytes()), "e": b64(e),
			})
		}
		json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// signJWT builds a token the way Cloudflare Access does.
func signJWT(t *testing.T, key *rsa.PrivateKey, kid, alg string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signing := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// claimsAt returns valid Access claims for a clock reading of now; mutate the
// copy to make one invalid.
func claimsAt(now time.Time) map[string]any {
	return map[string]any{
		"aud":   []string{testAUD},
		"iss":   "https://" + testTeam,
		"exp":   now.Add(time.Hour).Unix(),
		"nbf":   now.Add(-time.Minute).Unix(),
		"email": "me@example.com",
	}
}

// accessRig is a verifier pointed at a fake Cloudflare key endpoint.
type accessRig struct {
	v    *accessVerifier
	clk  *fakeClock
	hits *atomic.Int32
	srv  *httptest.Server
	keyA *rsa.PrivateKey
}

func newAccessRig(t *testing.T) *accessRig {
	t.Helper()
	a, _ := testKeys(t)
	srv, hits := jwks(t, map[string]*rsa.PublicKey{"kid-a": &a.PublicKey})
	clk := &fakeClock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	v := newAccessVerifier(testTeam, testAUD)
	v.certsURL = srv.URL
	v.client = srv.Client()
	v.now = clk.Now
	return &accessRig{v: v, clk: clk, hits: hits, srv: srv, keyA: a}
}

func (r *accessRig) token(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	c := claimsAt(r.clk.Now())
	if mutate != nil {
		mutate(c)
	}
	return signJWT(t, r.keyA, "kid-a", "RS256", c)
}

func TestAccessAcceptsAValidToken(t *testing.T) {
	r := newAccessRig(t)
	email, err := r.v.Verify(r.token(t, nil))
	if err != nil || email != "me@example.com" {
		t.Fatalf("Verify = %q, %v; want me@example.com, nil", email, err)
	}
}

func TestAccessAcceptsAStringAudience(t *testing.T) {
	r := newAccessRig(t)
	if _, err := r.v.Verify(r.token(t, func(c map[string]any) { c["aud"] = testAUD })); err != nil {
		t.Errorf("a plain-string aud should be accepted: %v", err)
	}
}

func TestAccessRejects(t *testing.T) {
	_, other := testKeys(t)
	r := newAccessRig(t)
	now := r.clk.Now()

	tests := []struct {
		name  string
		token func() string
	}{
		{"wrong audience", func() string { return r.token(t, func(c map[string]any) { c["aud"] = []string{"another-app"} }) }},
		{"no audience", func() string { return r.token(t, func(c map[string]any) { delete(c, "aud") }) }},
		{"wrong issuer", func() string {
			return r.token(t, func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" })
		}},
		{"expired", func() string { return r.token(t, func(c map[string]any) { c["exp"] = now.Add(-time.Second).Unix() }) }},
		{"no expiry", func() string { return r.token(t, func(c map[string]any) { delete(c, "exp") }) }},
		{"not valid yet", func() string {
			return r.token(t, func(c map[string]any) { c["nbf"] = now.Add(10 * time.Minute).Unix() })
		}},
		{"no email (a service token)", func() string { return r.token(t, func(c map[string]any) { delete(c, "email") }) }},
		{"signed with another key", func() string { return signJWT(t, other, "kid-a", "RS256", claimsAt(now)) }},
		{"unknown key id", func() string { return signJWT(t, r.keyA, "kid-zzz", "RS256", claimsAt(now)) }},
		{"algorithm none", func() string {
			h := b64([]byte(`{"alg":"none","kid":"kid-a"}`))
			p, _ := json.Marshal(claimsAt(now))
			return h + "." + b64(p) + "."
		}},
		{"HS256 (algorithm confusion)", func() string { return signJWT(t, r.keyA, "kid-a", "HS256", claimsAt(now)) }},
		{"two parts only", func() string { return "abc.def" }},
		{"garbage", func() string { return "!!!.@@@.###" }},
		{"empty", func() string { return "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			email, err := r.v.Verify(tc.token())
			if err == nil {
				t.Fatalf("accepted (as %q), want rejection", email)
			}
			if !errors.Is(err, errAccessDenied) {
				t.Errorf("error %v should wrap errAccessDenied, so callers can answer one uniform 403", err)
			}
		})
	}
}

func TestAccessAllowsALittleClockSkewOnNotBefore(t *testing.T) {
	r := newAccessRig(t)
	tok := r.token(t, func(c map[string]any) { c["nbf"] = r.clk.Now().Add(10 * time.Second).Unix() })
	if _, err := r.v.Verify(tok); err != nil {
		t.Errorf("10 s of skew should be tolerated: %v", err)
	}
}

func TestAccessCachesTheSigningKeys(t *testing.T) {
	r := newAccessRig(t)
	for i := 0; i < 3; i++ {
		if _, err := r.v.Verify(r.token(t, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.hits.Load(); n != 1 {
		t.Errorf("fetched the keys %d times for 3 verifications, want 1", n)
	}
}

// Cloudflare rotates keys: a token with a new key id must trigger a refresh, but
// a stream of bad ids must not make the Pi hammer Cloudflare.
func TestAccessRefreshesOnRotationButNotMoreOftenThanOncePerMinute(t *testing.T) {
	r := newAccessRig(t)
	a, _ := testKeys(t)
	if _, err := r.v.Verify(r.token(t, nil)); err != nil { // first fetch
		t.Fatal(err)
	}

	unknown := signJWT(t, a, "kid-new", "RS256", claimsAt(r.clk.Now()))
	if _, err := r.v.Verify(unknown); err == nil {
		t.Fatal("a token with an unknown key id was accepted")
	}
	if _, err := r.v.Verify(unknown); err == nil {
		t.Fatal("accepted")
	}
	if n := r.hits.Load(); n != 1 {
		t.Fatalf("%d key fetches within a minute of the last, want only the first", n)
	}

	// Cloudflare now publishes kid-new too; after the minute the refresh finds it.
	srv2, hits2 := jwks(t, map[string]*rsa.PublicKey{"kid-a": &a.PublicKey, "kid-new": &a.PublicKey})
	r.v.certsURL, r.v.client = srv2.URL, srv2.Client()
	r.clk.Advance(accessRefetchMin + time.Second)
	claims := claimsAt(r.clk.Now())
	if _, err := r.v.Verify(signJWT(t, a, "kid-new", "RS256", claims)); err != nil {
		t.Errorf("a rotated-in key should be picked up: %v", err)
	}
	if n := hits2.Load(); n != 1 {
		t.Errorf("%d fetches for the rotation, want 1", n)
	}
}

func TestAccessRefreshesStaleKeys(t *testing.T) {
	r := newAccessRig(t)
	if _, err := r.v.Verify(r.token(t, nil)); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(accessKeysTTL + time.Minute)
	if _, err := r.v.Verify(r.token(t, nil)); err != nil {
		t.Fatal(err)
	}
	if n := r.hits.Load(); n != 2 {
		t.Errorf("%d fetches, want a second after the cache expired", n)
	}
}

// If Cloudflare's keys can't be fetched, nobody gets in.
func TestAccessFailsClosedWhenTheKeysCannotBeFetched(t *testing.T) {
	a, _ := testKeys(t)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer broken.Close()
	clk := &fakeClock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	v := newAccessVerifier(testTeam, testAUD)
	v.certsURL, v.client, v.now = broken.URL, broken.Client(), clk.Now

	_, err := v.Verify(signJWT(t, a, "kid-a", "RS256", claimsAt(clk.Now())))
	if !errors.Is(err, errAccessDenied) {
		t.Errorf("error %v, want a denial", err)
	}
}

func TestAccessTokenSources(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if got := accessToken(r); got != "" {
		t.Errorf("token %q on a bare request", got)
	}
	r.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: "from-cookie"})
	if got := accessToken(r); got != "from-cookie" {
		t.Errorf("token %q, want the cookie's", got)
	}
	r.Header.Set("Cf-Access-Jwt-Assertion", "from-header")
	if got := accessToken(r); got != "from-header" {
		t.Errorf("token %q, want the header's to win", got)
	}
}
