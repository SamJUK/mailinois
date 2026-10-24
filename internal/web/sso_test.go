package web

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/SamJUK/mailinois/internal/alerts"
	"github.com/SamJUK/mailinois/internal/auth"
	"github.com/SamJUK/mailinois/internal/config"
	"github.com/SamJUK/mailinois/internal/ingest"
	"github.com/SamJUK/mailinois/internal/store/storetest"
)

// fakeIdP is an OpenID Connect provider: discovery, keys, and a token endpoint that hands out an ID token
// signed with a test key, for codes the test registers, after checking the PKCE verifier.
type fakeIdP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	mu    sync.Mutex
	codes map[string]idpCode
}

type idpCode struct {
	challenge string
	claims    map[string]any
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key, codes: map[string]idpCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		u := idp.srv.URL
		json.NewEncoder(w).Encode(map[string]any{"issuer": u, "authorization_endpoint": u + "/authorize", "token_endpoint": u + "/token",
			"jwks_uri": u + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"code"},
			"subject_types_supported": []string{"public"}})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		idp.mu.Lock()
		c, ok := idp.codes[r.PostForm.Get("code")]
		delete(idp.codes, r.PostForm.Get("code"))
		idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		id, secret, basic := r.BasicAuth()
		if !basic {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		if !ok || b64(sum[:]) != c.challenge || id != "mailinois" || secret != "client-secret" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 300, "id_token": idp.sign(c.claims)})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *fakeIdP) sign(claims map[string]any) string {
	head := b64([]byte(`{"alg":"RS256","kid":"test","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	signed := head + "." + b64(body)
	sum := sha256.Sum256([]byte(signed))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, sum[:])
	return signed + "." + b64(sig)
}

type ssoApp struct {
	url string
	idp *fakeIdP
	f   fixture
}

func newSSOApp(t *testing.T, oidc func(*config.OIDC), users ...config.User) ssoApp {
	idp := newIdP(t)
	st := storetest.SQLite(t)
	storetest.Load(t, st, "../store/testdata/scenarios.log")
	srv := httptest.NewUnstartedServer(nil)
	appURL := "http://" + srv.Listener.Addr().String()
	cfg := &config.Config{
		Sites:         []config.Site{{Name: "Acme", Slug: "acme", Domains: []string{"acme.test"}}, {Name: "Local", Slug: "local", Domains: []string{"example"}}},
		Auth:          config.Auth{Mode: "oidc", Users: users},
		QueueLifetime: config.Duration(100 * 365 * 24 * time.Hour),
		Location:      time.UTC,
	}
	cfg.Auth.OIDC = config.OIDC{Issuer: idp.srv.URL, ClientID: "mailinois", ClientSecret: "client-secret", RedirectURL: appURL + "/auth/callback",
		Scopes: []string{"openid", "email", "profile", "groups"}, GroupsClaim: "groups", ButtonLabel: "Sign in with Acme"}
	oidc(&cfg.Auth.OIDC)
	p := ingest.New(st, time.UTC, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	s := &Server{Store: st, Config: cfg, Auth: auth.New(cfg.Auth, "secret"), Pipeline: p, Version: "test",
		Alerts: &alerts.Evaluator{Store: st, Config: cfg}, LookupTXT: func(string) ([]string, error) { return nil, &net404{} }}
	srv.Config.Handler = s.Handler()
	srv.Start()
	t.Cleanup(srv.Close)
	return ssoApp{url: appURL, idp: idp, f: fixture{srv: srv, st: st}}
}

func goodClaims(iss, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{"iss": iss, "aud": "mailinois", "sub": "u-1", "email": "sam@example.com", "email_verified": true,
		"groups": []string{"staff"}, "nonce": nonce, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
}

type signIn struct {
	claims    func(c map[string]any) // changes the ID token's claims
	state     string                 // replaces the state in the callback
	freshJar  bool                   // returns without the flow cookie
	authorize url.Values             // filled in: what the provider was sent
}

// signIn runs the flow: Mailinois's redirect to the provider, the provider handing back a code, and the
// callback. It returns the callback's response and a client holding whatever cookies it set.
func (a ssoApp) signIn(t *testing.T, o signIn) (*http.Response, string, *http.Client) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Get(a.url + "/auth/login?next=/queue")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	to, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 302 || !strings.HasPrefix(to.String(), a.idp.srv.URL+"/authorize?") {
		t.Fatalf("/auth/login: %d to %s, want 302 to the provider", res.StatusCode, to)
	}
	q := to.Query()
	if o.authorize != nil {
		for k, v := range q {
			o.authorize[k] = v
		}
	}
	claims := goodClaims(a.idp.srv.URL, q.Get("nonce"))
	if o.claims != nil {
		o.claims(claims)
	}
	code := b64([]byte(time.Now().String()))
	a.idp.mu.Lock()
	a.idp.codes[code] = idpCode{challenge: q.Get("code_challenge"), claims: claims}
	a.idp.mu.Unlock()
	state := q.Get("state")
	if o.state != "" {
		state = o.state
	}
	if o.freshJar {
		jar, _ = cookiejar.New(nil)
		c.Jar = jar
	}
	res, err = c.Get(q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {state}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body), c
}

func sessionCookie(c *http.Client, raw string) bool {
	u, _ := url.Parse(raw)
	for _, k := range c.Jar.Cookies(u) {
		if k.Name == "mailinois_session" && k.Value != "" {
			return true
		}
	}
	return false
}

func TestSSOSignIn(t *testing.T) {
	a := newSSOApp(t, func(o *config.OIDC) { o.AllowedDomains = []string{"example.com"} })
	sent := url.Values{}
	res, _, c := a.signIn(t, signIn{authorize: sent})
	if res.StatusCode != 303 || res.Header.Get("Location") != "/queue" || !sessionCookie(c, a.url) {
		t.Fatalf("callback: %d to %q, session %v; want 303 to /queue and a session", res.StatusCode, res.Header.Get("Location"), sessionCookie(c, a.url))
	}
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if len(sent.Get(k)) < 20 {
			t.Errorf("the provider was sent %s=%q, want a long random value", k, sent.Get(k))
		}
	}
	if sent.Get("code_challenge_method") != "S256" || sent.Get("client_id") != "mailinois" || sent.Get("redirect_uri") != a.url+"/auth/callback" ||
		!strings.Contains(sent.Get("scope"), "openid") {
		t.Errorf("authorize request: %v", sent)
	}
	if code, body, _ := get(t, c, a.url+"/queue"); code != 200 || !strings.Contains(body, "Sign out sam@example.com") {
		t.Errorf("signed in, /queue: %d, signed-out link shown: %v", code, strings.Contains(body, "Sign out sam@example.com"))
	}
	if code, _, h := get(t, noRedirects(), a.url+"/queue"); code != 303 || !strings.HasPrefix(h.Get("Location"), "/login") {
		t.Errorf("no session, /queue: %d %s, want a redirect to /login", code, h.Get("Location"))
	}
}

func TestSSORefusesForgedOrStaleSignIns(t *testing.T) {
	for name, o := range map[string]signIn{
		"state from another sign-in": {state: "not-the-state"},
		"no flow cookie":             {freshJar: true},
		"token for another client":   {claims: func(c map[string]any) { c["aud"] = "someone-else" }},
		"token from another issuer":  {claims: func(c map[string]any) { c["iss"] = "https://evil.example" }},
		"expired token":              {claims: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
		"nonce from another sign-in": {claims: func(c map[string]any) { c["nonce"] = "not-the-nonce" }},
	} {
		// Each on its own app: more than five failures from one address would hit the sign-in limit instead.
		a := newSSOApp(t, func(o *config.OIDC) { o.AllowedDomains = []string{"example.com"} })
		if res, _, c := a.signIn(t, signIn{}); res.StatusCode != 303 || !sessionCookie(c, a.url) {
			t.Fatalf("%s, control: a good sign-in got %d", name, res.StatusCode)
		}
		res, body, c := a.signIn(t, o)
		if res.StatusCode != 400 || sessionCookie(c, a.url) || !strings.Contains(body, `class="error"`) {
			t.Errorf("%s: %d, session %v; want 400, the login page with an error, and no session", name, res.StatusCode, sessionCookie(c, a.url))
		}
	}
}

func TestSSOAllowedDomainsAndGroups(t *testing.T) {
	domains := newSSOApp(t, func(o *config.OIDC) { o.AllowedDomains = []string{"Example.com"} })
	groups := newSSOApp(t, func(o *config.OIDC) { o.AllowedGroups = []string{"mail-admins"} })
	for _, tc := range []struct {
		name   string
		app    ssoApp
		claims func(map[string]any)
		want   int
	}{
		{"verified address in the domain", domains, nil, 303},
		{"verified as the string true", domains, func(c map[string]any) { c["email_verified"] = "true" }, 303},
		{"address in another domain", domains, func(c map[string]any) { c["email"] = "eve@evil.example" }, 403},
		{"lookalike domain", domains, func(c map[string]any) { c["email"] = "eve@notexample.com" }, 403},
		{"unverified address", domains, func(c map[string]any) { c["email_verified"] = false }, 403},
		{"no email_verified claim", domains, func(c map[string]any) { delete(c, "email_verified") }, 403},
		{"in an allowed group", groups, func(c map[string]any) { c["groups"] = []string{"staff", "mail-admins"} }, 303},
		{"groups as one string", groups, func(c map[string]any) { c["groups"] = "mail-admins" }, 303},
		{"in no allowed group", groups, func(c map[string]any) { c["groups"] = []string{"staff"} }, 403},
		{"no groups claim", groups, func(c map[string]any) { delete(c, "groups") }, 403},
	} {
		res, _, c := tc.app.signIn(t, signIn{claims: tc.claims})
		if res.StatusCode != tc.want || sessionCookie(c, tc.app.url) != (tc.want == 303) {
			t.Errorf("%s: %d, session %v; want %d", tc.name, res.StatusCode, sessionCookie(c, tc.app.url), tc.want)
		}
	}

	listed := newSSOApp(t, func(*config.OIDC) {}, config.User{Name: "sam@example.com"})
	if res, _, _ := listed.signIn(t, signIn{}); res.StatusCode != 303 {
		t.Errorf("listed in auth.users: %d, want 303", res.StatusCode)
	}
	if res, _, _ := listed.signIn(t, signIn{claims: func(c map[string]any) { c["email"] = "ann@example.com" }}); res.StatusCode != 403 {
		t.Errorf("not listed in auth.users: %d, want 403", res.StatusCode)
	}
	// Anyone can type an address into a provider that doesn't verify it (the "nOAuth" attack on apps trusting
	// Entra ID's email claim): a listed address only counts once the provider vouches for it.
	for name, claims := range map[string]func(map[string]any){
		"listed address, unverified":    func(c map[string]any) { c["email_verified"] = false },
		"listed address, no such claim": func(c map[string]any) { delete(c, "email_verified") },
	} {
		if res, _, c := listed.signIn(t, signIn{claims: claims}); res.StatusCode != 403 || sessionCookie(c, listed.url) {
			t.Errorf("%s: %d, session %v; want 403 and none", name, res.StatusCode, sessionCookie(c, listed.url))
		}
	}
}

func TestSSOGroupSitesRestrictWhatTheySee(t *testing.T) {
	a := newSSOApp(t, func(o *config.OIDC) {
		o.AllowedDomains = []string{"example.com"}
		o.GroupSites = map[string][]string{"local-team": {"local"}}
	}, config.User{Name: "ann@example.com", Sites: []string{"local"}})
	acme, local := a.f.messageID(t, "4D5E6F7081"), a.f.messageID(t, "7A8B9C0D1E")

	for _, tc := range []struct {
		name       string
		claims     func(map[string]any)
		restricted bool
	}{
		{"mapped group", func(c map[string]any) { c["groups"] = []string{"local-team"} }, true},
		{"auth.users entry by email", func(c map[string]any) { c["email"], c["groups"] = "ann@example.com", nil }, true},
		{"neither", nil, false},
	} {
		res, _, c := a.signIn(t, signIn{claims: tc.claims})
		if res.StatusCode != 303 {
			t.Fatalf("%s: sign-in %d", tc.name, res.StatusCode)
		}
		code, body, _ := get(t, c, a.url+"/messages?period=30d")
		if code != 200 || !strings.Contains(body, "7A8B9C0D1E") || strings.Contains(body, "4D5E6F7081") != !tc.restricted {
			t.Errorf("%s: messages %d; sees acme.test: %v, sees the local pickup: %v", tc.name, code, strings.Contains(body, "4D5E6F7081"), strings.Contains(body, "7A8B9C0D1E"))
		}
		want := 200
		if tc.restricted {
			want = 404
		}
		if code, _, _ := get(t, c, a.url+"/messages/"+acme); code != want {
			t.Errorf("%s: a acme.test message: %d, want %d", tc.name, code, want)
		}
		if code, _, _ := get(t, c, a.url+"/messages/"+local); code != 200 {
			t.Errorf("%s: a local message: %d, want 200", tc.name, code)
		}
	}
}

func TestSSOBreakGlassPassword(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("break glass"), bcrypt.MinCost)
	withPassword := newSSOApp(t, func(o *config.OIDC) { o.AllowedDomains = []string{"example.com"} }, config.User{Name: "admin", PasswordHash: string(hash)})
	ssoOnly := newSSOApp(t, func(o *config.OIDC) { o.AllowedDomains = []string{"example.com"} })

	_, page, _ := get(t, noRedirects(), withPassword.url+"/login")
	if !strings.Contains(page, ">Sign in with Acme</a>") || !strings.Contains(page, `action="/login"`) {
		t.Errorf("login page with break-glass users: SSO button %v, password form %v", strings.Contains(page, "Sign in with Acme"), strings.Contains(page, `action="/login"`))
	}
	_, page, _ = get(t, noRedirects(), ssoOnly.url+"/login")
	if !strings.Contains(page, ">Sign in with Acme</a>") || strings.Contains(page, `action="/login"`) {
		t.Errorf("login page with SSO only: SSO button %v, password form %v", strings.Contains(page, "Sign in with Acme"), strings.Contains(page, `action="/login"`))
	}

	post := func(app ssoApp, password string) *http.Response {
		req, _ := http.NewRequest("POST", app.url+"/login", strings.NewReader(url.Values{"name": {"admin"}, "password": {password}, "next": {"/queue"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", app.url)
		res, err := noRedirects().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := post(withPassword, "wrong"); res.StatusCode != 401 {
		t.Errorf("break-glass, wrong password: %d, want 401", res.StatusCode)
	}
	res := post(withPassword, "break glass")
	if res.StatusCode != 303 || res.Header.Get("Location") != "/queue" || len(res.Cookies()) == 0 {
		t.Fatalf("break-glass, right password: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(withPassword.url)
	jar.SetCookies(u, res.Cookies())
	if code, _, _ := get(t, &http.Client{Jar: jar}, withPassword.url+"/queue"); code != 200 {
		t.Errorf("break-glass session, /queue: %d", code)
	}
	if res := post(ssoOnly, "break glass"); res.StatusCode != 303 || res.Header.Get("Location") != "/login" || len(res.Cookies()) != 0 {
		t.Errorf("password form with SSO only: %d to %s, %d cookies; want back to /login and no session", res.StatusCode, res.Header.Get("Location"), len(res.Cookies()))
	}
}

func TestSSOFailuresCountTowardsTheSignInLimit(t *testing.T) {
	a := newSSOApp(t, func(o *config.OIDC) { o.AllowedDomains = []string{"example.com"} })
	if res, _, _ := a.signIn(t, signIn{}); res.StatusCode != 303 {
		t.Fatalf("control: %d", res.StatusCode)
	}
	for i := range 5 {
		if res, _, _ := a.signIn(t, signIn{claims: func(c map[string]any) { c["email"] = "eve@evil.example" }}); res.StatusCode != 403 {
			t.Fatalf("denied sign-in %d: %d, want 403", i+1, res.StatusCode)
		}
	}
	if res, _, c := a.signIn(t, signIn{}); res.StatusCode != 429 || sessionCookie(c, a.url) {
		t.Errorf("good sign-in after 5 failures: %d, want 429 and no session", res.StatusCode)
	}
}

func TestSSONextStaysLocal(t *testing.T) {
	a := newSSOApp(t, func(o *config.OIDC) { o.AllowedDomains = []string{"example.com"} })
	for _, next := range []string{"//evil.example", "/\\evil.example", "https://evil.example"} {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		res, err := c.Get(a.url + "/auth/login?next=" + url.QueryEscape(next))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		q := mustURL(t, res.Header.Get("Location")).Query()
		code := "code-" + b64([]byte(next))
		a.idp.mu.Lock()
		a.idp.codes[code] = idpCode{challenge: q.Get("code_challenge"), claims: goodClaims(a.idp.srv.URL, q.Get("nonce"))}
		a.idp.mu.Unlock()
		res, err = c.Get(q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 303 || res.Header.Get("Location") != "/" {
			t.Errorf("next=%s: %d to %q, want 303 to /", next, res.StatusCode, res.Header.Get("Location"))
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
