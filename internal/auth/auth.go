// Package auth decides who is asking: nobody in particular (auth off), a signed-in user, a user named by a
// trusted proxy, or an API token. Everything comes from the config file; sessions are signed cookies.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/SamJUK/mailinois/internal/config"
)

const (
	cookieName = "mailinois_session"
	sessionFor = 12 * time.Hour
)

// Viewer is whoever made the request.
type Viewer struct {
	Name   string
	Sites  []string // nil: every site
	Scopes []string // API tokens only
	Token  bool
}

func (v *Viewer) Can(scope string) bool { return !v.Token || slices.Contains(v.Scopes, scope) }

type Auth struct {
	cfg    config.Auth
	secret []byte

	mu       sync.Mutex
	failures map[string][]time.Time // sign-in failures by client address

	sso ssoProvider
}

func New(cfg config.Auth, secret string) *Auth {
	a := &Auth{cfg: cfg, secret: []byte(secret), failures: map[string][]time.Time{}}
	if cfg.Mode == "oidc" {
		go a.discoverAtStart()
	}
	return a
}

const (
	maxFailures    = 5  // from one client
	maxAllFailures = 50 // from everyone together: a guess from many addresses at once
	failWindow     = 15 * time.Minute
	everyone       = "*"
)

// Client is the address a request came from. Through trusted proxies it is the last X-Forwarded-For entry
// that isn't one of them: each proxy appends who it heard from, and anything further left is whatever the
// client chose to send.
func (a *Auth) Client(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !inPrefixes(a.cfg.ForwardedPrefixes, host) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		if hop := strings.TrimSpace(hops[i]); hop != "" && !inPrefixes(a.cfg.ForwardedPrefixes, hop) {
			return hop
		}
	}
	return host
}

// Blocked reports whether a client, or everyone together, has failed to sign in too often lately.
func (a *Auth) Blocked(client string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.recent(client)) >= maxFailures || len(a.recent(everyone)) >= maxAllFailures
}

// Failed records a failed sign-in.
func (a *Auth) Failed(client string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures[client] = append(a.recent(client), time.Now())
	a.failures[everyone] = append(a.recent(everyone), time.Now())
	if len(a.failures) > 10_000 { // someone spraying addresses: forget the oldest rather than grow
		for k := range a.failures {
			if len(a.recent(k)) == 0 {
				delete(a.failures, k)
			}
		}
	}
}

func (a *Auth) recent(client string) []time.Time {
	cutoff := time.Now().Add(-failWindow)
	kept := a.failures[client][:0]
	for _, t := range a.failures[client] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(a.failures, client)
		return nil
	}
	a.failures[client] = kept
	return kept
}

func (a *Auth) Mode() string { return a.cfg.Mode }

type ctxKey struct{}

// From returns the viewer the middleware attached.
func From(ctx context.Context) *Viewer {
	v, _ := ctx.Value(ctxKey{}).(*Viewer)
	return v
}

func with(r *http.Request, v *Viewer) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, v))
}

// Viewer works out who is asking, or nil when nobody acceptable is. A bearer token always wins.
func (a *Auth) Viewer(r *http.Request) *Viewer {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return a.token(strings.TrimPrefix(h, "Bearer "))
	}
	switch a.cfg.Mode {
	case "off":
		return &Viewer{Name: "anyone"}
	case "password", "oidc":
		c, err := r.Cookie(cookieName)
		if err != nil {
			return nil
		}
		s, ok := a.verify(c.Value)
		switch {
		case !ok:
		case s.sso && a.cfg.Mode == "oidc":
			return &Viewer{Name: s.name, Sites: s.sites}
		case !s.sso:
			return a.user(s.name) // a password session, or OIDC's break-glass login
		}
	case "proxy":
		if name := strings.TrimSpace(r.Header.Get(a.cfg.Proxy.UserHeader)); name != "" && a.trusted(r.RemoteAddr) {
			if v := a.user(name); v != nil {
				return v
			}
			return &Viewer{Name: name} // the proxy signed them in; the config only narrows what they see
		}
	}
	return nil
}

// Page requires a viewer for a dashboard page: signed-in users, the proxy's users, or anyone when auth is off.
// Password and OIDC modes send everyone else to the login page; proxy mode answers 401, since the proxy should
// have asked already.
func (a *Auth) Page(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := a.Viewer(r)
		if v == nil || v.Token {
			if a.cfg.Mode == "password" || a.cfg.Mode == "oidc" {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "Unauthorized: sign in through your proxy", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, with(r, v))
	})
}

// API requires a viewer with scope: a token that has it, or anyone a page would let in.
func (a *Auth) API(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := a.Viewer(r)
		switch {
		case v == nil:
			w.Header().Set("WWW-Authenticate", `Bearer realm="Mailinois"`)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		case scope == "ingest" && !v.Token:
			// Even with auth off: anything that can write lines needs a token of its own.
			http.Error(w, `{"error":"ingest needs an API token with the ingest scope"}`, http.StatusForbidden)
		case !v.Can(scope):
			http.Error(w, `{"error":"this token can't `+scope+`"}`, http.StatusForbidden)
		default:
			next.ServeHTTP(w, with(r, v))
		}
	})
}

func (a *Auth) user(name string) *Viewer {
	for _, u := range a.cfg.Users {
		if u.Name == name {
			return &Viewer{Name: u.Name, Sites: nonNil(u.Sites)}
		}
	}
	return nil
}

// nonNil turns an empty restriction into "every site".
func nonNil(sites []string) []string {
	if len(sites) == 0 {
		return nil
	}
	return sites
}

func (a *Auth) token(t string) *Viewer {
	sum := sha256.Sum256([]byte(t))
	hash := "sha256:" + hex.EncodeToString(sum[:])
	for _, tok := range a.cfg.Tokens {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(tok.Hash)) == 1 {
			return &Viewer{Name: tok.Name, Sites: nonNil(tok.Sites), Scopes: tok.Scopes, Token: true}
		}
	}
	return nil
}

func (a *Auth) trusted(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	return inPrefixes(a.cfg.TrustedPrefixes, host)
}

func inPrefixes(prefixes []netip.Prefix, addr string) bool {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return slices.ContainsFunc(prefixes, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// dummyHash makes a sign-in with an unknown name cost as much as one with a wrong password.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not anyone's password"), bcrypt.DefaultCost)

// Login checks a name and password.
func (a *Auth) Login(name, password string) bool {
	hash, found := dummyHash, false
	for _, u := range a.cfg.Users {
		if u.Name == name {
			hash, found = []byte(u.PasswordHash), true
		}
	}
	ok := bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	return found && ok
}

// SetSession signs a password user in on this response.
func (a *Auth) SetSession(w http.ResponseWriter, r *http.Request, name string) {
	a.setSession(w, r, name, "")
}

// setSSOSession signs in someone the OIDC provider vouched for. Their sites are decided at sign-in, from
// their groups, so they travel in the cookie: name.expiry.sites, sites empty for every site.
func (a *Auth) setSSOSession(w http.ResponseWriter, r *http.Request, name string, sites []string) {
	a.setSession(w, r, name, "."+base64.RawURLEncoding.EncodeToString([]byte(strings.Join(sites, ","))))
}

func (a *Auth) setSession(w http.ResponseWriter, r *http.Request, name, sites string) {
	expires := time.Now().Add(sessionFor)
	payload := base64.RawURLEncoding.EncodeToString([]byte(name)) + "." + strconv.FormatInt(expires.Unix(), 10) + sites
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: payload + "." + a.sign(payload), Path: "/", Expires: expires,
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (a *Auth) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func (a *Auth) sign(payload string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

type session struct {
	name  string
	sso   bool
	sites []string // sso only; nil is every site
}

func (a *Auth) verify(cookie string) (session, bool) {
	i := strings.LastIndexByte(cookie, '.')
	if i < 0 || !hmac.Equal([]byte(cookie[i+1:]), []byte(a.sign(cookie[:i]))) {
		return session{}, false
	}
	parts := strings.Split(cookie[:i], ".")
	if len(parts) != 2 && len(parts) != 3 {
		return session{}, false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return session{}, false
	}
	name, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return session{}, false
	}
	s := session{name: string(name), sso: len(parts) == 3}
	if s.sso {
		sites, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			return session{}, false
		}
		if len(sites) > 0 {
			s.sites = strings.Split(string(sites), ",")
		}
	}
	return s, true
}

// NewSecret is a random session secret.
func NewSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewToken returns a new API token and the hash to put in the config.
func NewToken() (token, hash string) {
	b := make([]byte, 24)
	rand.Read(b)
	token = "mlns_" + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, "sha256:" + hex.EncodeToString(sum[:])
}
