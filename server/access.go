package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Cloudflare Access verification.
//
// With the dashboard behind a Cloudflare Tunnel, Cloudflare Access is the outer
// gate: a visitor who hasn't logged in is turned away at Cloudflare's edge and
// never reaches the Pi. This is the inner gate. The app does not trust the
// Cf-Access-Jwt-Assertion header for being there; it checks the proof inside it
// (Cloudflare's signature, this application's audience, issuer, expiry), so a
// mistake in the Access application's rules, or a request that reaches the
// tunnel some other way, does not silently expose a page that can start a car.

const (
	// accessKeysTTL: Cloudflare rotates its signing keys, so the cache is short
	// enough that a rotation heals by itself, long enough to spare a fetch per request.
	accessKeysTTL = time.Hour

	// accessRefetchMin bounds how often a refresh may be attempted, so a stream
	// of garbage tokens can't make the Pi hammer Cloudflare.
	accessRefetchMin = time.Minute

	// accessLeeway tolerates a little clock skew on "not before".
	accessLeeway = 30 * time.Second
)

// errAccessDenied is what callers see for every failure, on purpose: telling a
// stranger which check failed is free help. The specific reason is logged.
var errAccessDenied = errors.New("access denied")

// accessVerifier checks Cloudflare Access JWTs for one Access application.
type accessVerifier struct {
	team     string // "yourteam.cloudflareaccess.com"
	aud      string // the application's Audience (AUD) tag
	certsURL string
	client   *http.Client
	now      func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	lastTry   time.Time
}

func newAccessVerifier(team, aud string) *accessVerifier {
	return &accessVerifier{
		team:     team,
		aud:      aud,
		certsURL: "https://" + team + "/cdn-cgi/access/certs",
		client:   &http.Client{Timeout: 5 * time.Second},
		now:      time.Now,
	}
}

// accessToken returns the Access token a request carries: the header Access
// adds at the edge, or the CF_Authorization cookie.
func accessToken(r *http.Request) string {
	if t := r.Header.Get("Cf-Access-Jwt-Assertion"); t != "" {
		return t
	}
	if c, err := r.Cookie("CF_Authorization"); err == nil {
		return c.Value
	}
	return ""
}

// Verify checks the token and returns the email Access authenticated. Any
// failure — no token, wrong algorithm, unknown key, bad signature, wrong
// audience or issuer, expired, no email — returns an error wrapping
// errAccessDenied; the detail is for the log.
func (v *accessVerifier) Verify(token string) (string, error) {
	deny := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("%w: %s", errAccessDenied, fmt.Sprintf(format, args...))
	}
	if token == "" {
		return deny("no token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return deny("malformed token")
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeJWTPart(parts[0], &header); err != nil {
		return deny("bad header: %v", err)
	}
	// Only RS256: accepting whatever the token claims ("none", HS256 keyed with
	// the public key) is the classic way to forge one.
	if header.Alg != "RS256" {
		return deny("unsupported algorithm %q", header.Alg)
	}

	pub, err := v.key(header.Kid)
	if err != nil {
		return deny("signing key: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return deny("bad signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return deny("signature does not verify")
	}

	var claims struct {
		Aud   json.RawMessage `json:"aud"`
		Iss   string          `json:"iss"`
		Exp   *float64        `json:"exp"`
		Nbf   *float64        `json:"nbf"`
		Email string          `json:"email"`
	}
	if err := decodeJWTPart(parts[1], &claims); err != nil {
		return deny("bad claims: %v", err)
	}

	// aud ties the token to this application; without it any token from the same
	// Access team would open this dashboard.
	if !audienceIncludes(claims.Aud, v.aud) {
		return deny("audience does not match")
	}
	if claims.Iss != "https://"+v.team {
		return deny("issuer %q", claims.Iss)
	}
	now := v.now()
	if claims.Exp == nil || !now.Before(unixTime(*claims.Exp)) {
		return deny("expired")
	}
	if claims.Nbf != nil && unixTime(*claims.Nbf).After(now.Add(accessLeeway)) {
		return deny("not valid yet")
	}
	if claims.Email == "" {
		return deny("no email in token")
	}
	return claims.Email, nil
}

func unixTime(sec float64) time.Time {
	return time.Unix(int64(sec), 0)
}

func decodeJWTPart(part string, into any) error {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

// audienceIncludes reports whether the aud claim (a string or a list) contains want.
func audienceIncludes(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

// key returns the signing key with the given id, refreshing the cached set when
// it is stale or the id is unknown (a rotation), at most once per accessRefetchMin.
// If the refresh fails the answer is "no key": fail closed.
func (v *accessVerifier) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.now()
	k, known := v.keys[kid]
	if known && now.Sub(v.fetchedAt) < accessKeysTTL {
		return k, nil
	}
	if !v.lastTry.IsZero() && now.Sub(v.lastTry) < accessRefetchMin {
		if known {
			return k, nil // stale, but we only just tried: keep using it
		}
		return nil, errors.New("unknown key id")
	}
	v.lastTry = now
	keys, err := v.fetchKeys()
	if err != nil {
		return nil, err
	}
	v.keys, v.fetchedAt = keys, now
	k, known = keys[kid]
	if !known {
		return nil, errors.New("unknown key id")
	}
	return k, nil
}

// fetchKeys downloads the team's public signing keys (a JWK set).
func (v *accessVerifier) fetchKeys() (map[string]*rsa.PublicKey, error) {
	resp, err := v.client.Get(v.certsURL)
	if err != nil {
		return nil, fmt.Errorf("fetch signing keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch signing keys: HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("parse signing keys: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	}
	if len(keys) == 0 {
		return nil, errors.New("fetch signing keys: none usable")
	}
	return keys, nil
}
