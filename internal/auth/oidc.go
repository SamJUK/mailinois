package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/SamJUK/mailinois/internal/config"
)

const (
	flowCookie      = "mailinois_oidc"
	flowFor         = 10 * time.Minute
	discoverTimeout = 10 * time.Second
)

// ssoProvider is the provider's discovery document, fetched once it answers.
type ssoProvider struct {
	mu       sync.Mutex
	provider *oidc.Provider
}

// SSO reports whether sign-in goes through an OIDC provider, and the button's label.
func (a *Auth) SSO() (bool, string) { return a.cfg.Mode == "oidc", a.cfg.OIDC.ButtonLabel }

// Passwords reports whether the login page takes a name and password: password mode, or OIDC's break-glass
// users with a password_hash.
func (a *Auth) Passwords() bool {
	if a.cfg.Mode == "password" {
		return true
	}
	return a.cfg.Mode == "oidc" && slices.ContainsFunc(a.cfg.Users, func(u config.User) bool { return u.PasswordHash != "" })
}

func (a *Auth) discoverAtStart() {
	if _, err := a.provider(context.Background()); err != nil {
		slog.Warn("can't reach the OIDC provider; starting anyway, and trying again at the next sign-in", "issuer", a.cfg.OIDC.Issuer, "err", err)
		return
	}
	slog.Info("OIDC provider found", "issuer", a.cfg.OIDC.Issuer)
}

func (a *Auth) provider(ctx context.Context) (*oidc.Provider, error) {
	a.sso.mu.Lock()
	defer a.sso.mu.Unlock()
	if a.sso.provider != nil {
		return a.sso.provider, nil
	}
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	p, err := oidc.NewProvider(ctx, a.cfg.OIDC.Issuer)
	if err != nil {
		return nil, err
	}
	a.sso.provider = p
	return p, nil
}

func (a *Auth) oauth(p *oidc.Provider) *oauth2.Config {
	o := a.cfg.OIDC
	return &oauth2.Config{ClientID: o.ClientID, ClientSecret: o.ClientSecret, RedirectURL: o.RedirectURL, Endpoint: p.Endpoint(), Scopes: o.Scopes}
}

// flow is what the browser carries between the redirect to the provider and its return, in a signed cookie.
type flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
}

// StartSSO sets the flow cookie and returns the provider's sign-in URL, with state, nonce and a PKCE challenge.
func (a *Auth) StartSSO(w http.ResponseWriter, r *http.Request, next string) (string, error) {
	p, err := a.provider(r.Context())
	if err != nil {
		return "", err
	}
	f := flow{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Next: next, Expires: time.Now().Add(flowFor).Unix()}
	raw, _ := json.Marshal(f)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: payload + "." + a.sign("flow:"+payload), Path: "/auth/",
		MaxAge: int(flowFor.Seconds()), HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
	return a.oauth(p).AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), nil
}

// SSOError is a sign-in that didn't work: what to show, its status, and why, for the log.
type SSOError struct {
	Status  int
	Message string
	Reason  string
}

func (e *SSOError) Error() string { return e.Reason }

func ssoErr(status int, message, reason string, args ...any) *SSOError {
	return &SSOError{Status: status, Message: message, Reason: fmt.Sprintf(reason, args...)}
}

// FinishSSO handles the provider's return: it checks the flow cookie and state, swaps the code for tokens,
// verifies the ID token (signature, issuer, audience, expiry) and its nonce, applies allowed_domains,
// allowed_groups and auth.users, and signs the person in. It returns where to send them next.
func (a *Auth) FinishSSO(w http.ResponseWriter, r *http.Request) (string, error) {
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: "", Path: "/auth/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	f, ok := a.readFlow(r)
	if !ok {
		return "", ssoErr(400, "That sign-in had expired or came from somewhere else. Try again.", "no valid flow cookie")
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 {
		return "", ssoErr(400, "That sign-in had expired or came from somewhere else. Try again.", "state doesn't match")
	}
	if e := q.Get("error"); e != "" {
		return "", ssoErr(403, "The sign-in provider didn't sign you in.", "provider said %s: %s", e, q.Get("error_description"))
	}
	p, err := a.provider(r.Context())
	if err != nil {
		return "", ssoErr(502, "Mailinois can't reach the sign-in provider. Try again shortly.", "discovery: %v", err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), discoverTimeout)
	defer cancel()
	token, err := a.oauth(p).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return "", ssoErr(400, "The sign-in provider didn't accept that sign-in. Try again.", "code exchange: %v", err)
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		return "", ssoErr(400, "The sign-in provider didn't send an ID token.", "no id_token in the token response")
	}
	id, err := p.Verifier(&oidc.Config{ClientID: a.cfg.OIDC.ClientID}).Verify(ctx, raw)
	if err != nil {
		return "", ssoErr(400, "The sign-in provider's answer didn't check out. Try again.", "id token: %v", err)
	}
	if subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(f.Nonce)) != 1 {
		return "", ssoErr(400, "The sign-in provider's answer didn't check out. Try again.", "id token nonce doesn't match")
	}
	var claims map[string]any
	if err := id.Claims(&claims); err != nil {
		return "", ssoErr(400, "The sign-in provider's answer didn't check out. Try again.", "id token claims: %v", err)
	}
	name, sites, err := a.admit(claims)
	if err != nil {
		return "", err
	}
	a.setSSOSession(w, r, name, sites)
	return f.Next, nil
}

func (a *Auth) readFlow(r *http.Request) (flow, bool) {
	c, err := r.Cookie(flowCookie)
	if err != nil {
		return flow{}, false
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.sign("flow:"+payload))) {
		return flow{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	var f flow
	if err != nil || json.Unmarshal(raw, &f) != nil || time.Now().Unix() > f.Expires {
		return flow{}, false
	}
	return f, true
}

// admit decides whether the ID token's person may sign in, and which sites they see: the sites their groups
// map to in group_sites; else those of the auth.users entry named by their email; else every site.
func (a *Auth) admit(claims map[string]any) (string, []string, error) {
	o := a.cfg.OIDC
	email, _ := claims["email"].(string)
	email = strings.ToLower(email)
	name := email
	for _, k := range []string{"preferred_username", "sub"} {
		if s, _ := claims[k].(string); name == "" {
			name = s
		}
	}
	groups := stringsClaim(claims[o.GroupsClaim])
	// An auth.users entry names someone by email, so it only counts when the provider vouches for the address:
	// otherwise anyone who can type it into a lax provider is that person (the "nOAuth" attack).
	user := -1
	if email != "" && verified(claims["email_verified"]) {
		user = slices.IndexFunc(a.cfg.Users, func(u config.User) bool { return strings.EqualFold(u.Name, email) })
	}

	if len(o.AllowedDomains) > 0 {
		_, domain, _ := strings.Cut(email, "@")
		if !slices.ContainsFunc(o.AllowedDomains, func(d string) bool { return strings.EqualFold(d, domain) }) {
			return "", nil, ssoErr(403, "Your account isn't in a domain allowed to sign in here.", "%q: domain not in allowed_domains", name)
		}
		if !verified(claims["email_verified"]) {
			return "", nil, ssoErr(403, "Your account's email address isn't verified with the sign-in provider.", "%q: email_verified isn't true", name)
		}
	}
	if len(o.AllowedGroups) > 0 && !slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(o.AllowedGroups, g) }) {
		return "", nil, ssoErr(403, "Your account isn't in a group allowed to sign in here.", "%q: in none of allowed_groups (has %v)", name, groups)
	}
	if len(o.AllowedDomains) == 0 && len(o.AllowedGroups) == 0 && user < 0 {
		return "", nil, ssoErr(403, "Your account isn't on the list allowed to sign in here.", "%q: not in auth.users", name)
	}
	if name == "" {
		return "", nil, ssoErr(400, "The sign-in provider didn't say who you are.", "no email, preferred_username or sub")
	}

	var sites []string
	for _, g := range groups {
		for _, s := range o.GroupSites[g] {
			if !slices.Contains(sites, s) {
				sites = append(sites, s)
			}
		}
	}
	if len(sites) == 0 && user >= 0 {
		sites = a.cfg.Users[user].Sites
	}
	return name, sites, nil
}

// stringsClaim reads a claim that is a list of strings, or one string.
func stringsClaim(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// verified reads email_verified, which some providers send as a string.
func verified(v any) bool {
	b, ok := v.(bool)
	return (ok && b) || v == "true"
}

func random() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AsSSOError is a sign-in failure as the login page shows it.
func AsSSOError(err error) *SSOError {
	var e *SSOError
	if errors.As(err, &e) {
		return e
	}
	return &SSOError{Status: http.StatusInternalServerError, Message: "Sign-in didn't work. Try again.", Reason: err.Error()}
}
