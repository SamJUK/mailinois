package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/SamJUK/mailinois/internal/config"
)

func newAuth(t *testing.T, mode string) (*Auth, string) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	token, tokenHash := NewToken()
	cfg := config.Auth{Mode: mode, Users: []config.User{{Name: "sam", PasswordHash: string(hash)}, {Name: "ann", PasswordHash: string(hash), Sites: []string{"storea"}}},
		Tokens:            []config.Token{{Name: "grafana", Hash: tokenHash, Scopes: []string{"read"}}},
		TrustedPrefixes:   []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		ForwardedPrefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.64.0.0/13")}}
	cfg.Proxy.UserHeader = "Remote-User"
	return New(cfg, "secret"), token
}

func TestPasswordSessions(t *testing.T) {
	a, _ := newAuth(t, "password")
	if a.Login("sam", "wrong") || a.Login("nobody", "s3cret") || !a.Login("sam", "s3cret") {
		t.Fatal("Login accepted a wrong password or name, or refused the right one")
	}
	rec := httptest.NewRecorder()
	a.SetSession(rec, httptest.NewRequest("POST", "/login", nil), "ann")
	cookie := rec.Result().Cookies()[0]

	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	if v := a.Viewer(r); v == nil || v.Name != "ann" || len(v.Sites) != 1 || v.Sites[0] != "storea" {
		t.Errorf("signed-in viewer = %+v", v)
	}
	tampered := *cookie
	tampered.Value = strings.Replace(cookie.Value, cookie.Value[:4], "c2Ft", 1) // "sam" in base64
	r = httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&tampered)
	if v := a.Viewer(r); v != nil {
		t.Errorf("a tampered cookie signed in %+v", v)
	}
	if v := a.Viewer(httptest.NewRequest("GET", "/", nil)); v != nil {
		t.Errorf("no cookie signed in %+v", v)
	}
}

func TestProxy(t *testing.T) {
	a, _ := newAuth(t, "proxy")
	for _, tc := range []struct {
		remote, user string
		want         string
	}{
		{"10.1.0.4:5555", "ann", "ann"},
		{"10.1.0.4:5555", "someone", "someone"},
		{"203.0.113.9:5555", "ann", ""}, // not the proxy: the header means nothing
		{"10.1.0.4:5555", "", ""},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		if tc.user != "" {
			r.Header.Set("Remote-User", tc.user)
		}
		v := a.Viewer(r)
		if got := ""; v != nil {
			got = v.Name
			if got != tc.want {
				t.Errorf("%s as %q: viewer %q, want %q", tc.remote, tc.user, got, tc.want)
			}
		} else if tc.want != "" {
			t.Errorf("%s as %q: no viewer, want %q", tc.remote, tc.user, tc.want)
		}
	}
}

func TestTokensAndMiddleware(t *testing.T) {
	a, token := newAuth(t, "password")
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(From(r.Context()).Name)) })
	for _, tc := range []struct {
		name    string
		handler http.Handler
		bearer  string
		want    int
	}{
		{"read with a read token", a.API("read", ok), token, 200},
		{"ingest with a read token", a.API("ingest", ok), token, 403},
		{"read with a wrong token", a.API("read", ok), "mlns_nope", 401},
		{"read with nothing", a.API("read", ok), "", 401},
		{"a page with a token", a.Page(ok), token, 303},
		{"a page with nothing", a.Page(ok), "", 303},
	} {
		r := httptest.NewRequest("GET", "/x", nil)
		if tc.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		rec := httptest.NewRecorder()
		tc.handler.ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, rec.Code, tc.want)
		}
	}

	off, offToken := newAuth(t, "off")
	for _, tc := range []struct {
		bearer string
		want   int
	}{{"", 403}, {offToken, 403}} {
		r := httptest.NewRequest("POST", "/api/v1/ingest", nil)
		if tc.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		rec := httptest.NewRecorder()
		off.API("ingest", ok).ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("auth off, ingest with token %q: %d, want %d", tc.bearer, rec.Code, tc.want)
		}
	}
	ingest, ingestHash := NewToken()
	withAgent := New(config.Auth{Mode: "off", Tokens: []config.Token{{Name: "agent", Hash: ingestHash, Scopes: []string{"ingest"}}}}, "secret")
	r := httptest.NewRequest("POST", "/api/v1/ingest", nil)
	r.Header.Set("Authorization", "Bearer "+ingest)
	rec := httptest.NewRecorder()
	withAgent.API("ingest", ok).ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Errorf("auth off, ingest with an ingest token: %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	off.Page(ok).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || rec.Body.String() != "anyone" {
		t.Errorf("auth off: %d %q", rec.Code, rec.Body.String())
	}
}

func TestClientOnlyTrustsForwardedFromProxies(t *testing.T) {
	a, _ := newAuth(t, "password")
	r := httptest.NewRequest("POST", "/login", nil)
	r.Header.Set("X-Forwarded-For", "198.51.100.7, 10.1.0.2")
	r.RemoteAddr = "203.0.113.9:4444"
	if got := a.Client(r); got != "203.0.113.9" {
		t.Errorf("untrusted connection: client %q, want its own address (the header is the attacker's)", got)
	}
	r.RemoteAddr = "10.1.0.2:4444"
	if got := a.Client(r); got != "198.51.100.7" {
		t.Errorf("trusted proxy: client %q, want the forwarded address", got)
	}
	// Through Cloudflare, then Traefik: whatever the client wrote, the client Cloudflare saw, the Cloudflare edge.
	r.Header.Set("X-Forwarded-For", "192.0.2.66, 198.51.100.7, 172.64.1.1")
	if got := a.Client(r); got != "198.51.100.7" {
		t.Errorf("through two proxies: client %q, want the one Cloudflare saw, not the one the client wrote", got)
	}
	r.Header.Set("X-Forwarded-For", "10.1.0.5")
	if got := a.Client(r); got != "10.1.0.2" {
		t.Errorf("only proxies forwarded: client %q, want the connection's address", got)
	}
}

func TestSignInLimitAcrossClients(t *testing.T) {
	a, _ := newAuth(t, "password")
	for i := range maxAllFailures - 1 {
		a.Failed(fmt.Sprintf("192.0.2.%d", i))
	}
	if a.Blocked("198.51.100.7") {
		t.Fatalf("%d failures from other addresses already block a new one", maxAllFailures-1)
	}
	if a.Blocked("192.0.2.1") {
		t.Errorf("one failure blocked its own client")
	}
	a.Failed("192.0.2.250")
	if !a.Blocked("198.51.100.7") {
		t.Errorf("%d failures from many addresses: a new one isn't blocked", maxAllFailures)
	}
}

func TestSSOSessionsStayInOIDCMode(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	users := []config.User{{Name: "admin", PasswordHash: string(hash)}}
	sso := New(config.Auth{Mode: "oidc", Users: users}, "secret")
	password := New(config.Auth{Mode: "password", Users: users}, "secret")
	cookie := func(set func(http.ResponseWriter, *http.Request)) *http.Cookie {
		rec := httptest.NewRecorder()
		set(rec, httptest.NewRequest("GET", "/", nil))
		return rec.Result().Cookies()[0]
	}
	viewer := func(a *Auth, c *http.Cookie) *Viewer {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(c)
		return a.Viewer(r)
	}

	ssoCookie := cookie(func(w http.ResponseWriter, r *http.Request) {
		sso.setSSOSession(w, r, "ann@example.com", []string{"storea", "storeb"})
	})
	if v := viewer(sso, ssoCookie); v == nil || v.Name != "ann@example.com" || strings.Join(v.Sites, ",") != "storea,storeb" {
		t.Errorf("SSO session in OIDC mode: %+v, want ann with storea and storeb", v)
	}
	if v := viewer(password, ssoCookie); v != nil {
		t.Errorf("SSO session in password mode: %+v, want nobody", v)
	}
	every := cookie(func(w http.ResponseWriter, r *http.Request) { sso.setSSOSession(w, r, "sam@example.com", nil) })
	if v := viewer(sso, every); v == nil || v.Sites != nil {
		t.Errorf("SSO session for every site: %+v, want nil sites", v)
	}
	glass := cookie(func(w http.ResponseWriter, r *http.Request) { sso.SetSession(w, r, "admin") })
	if v := viewer(sso, glass); v == nil || v.Name != "admin" {
		t.Errorf("break-glass session in OIDC mode: %+v, want admin", v)
	}
	ghost := cookie(func(w http.ResponseWriter, r *http.Request) { sso.SetSession(w, r, "nobody") })
	if v := viewer(sso, ghost); v != nil {
		t.Errorf("password session for a user who isn't configured: %+v, want nobody", v)
	}
}
