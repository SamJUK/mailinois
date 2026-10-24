package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, yaml string) string {
	path := filepath.Join(t.TempDir(), "mailinois.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExpandsEnvironment(t *testing.T) {
	t.Setenv("ALERT_HOOK", "https://hooks.slack.com/services/T0/B0/x")
	c, err := Load(write(t, `
# Secrets come from ${THE_ENVIRONMENT}: this comment is not expanded.
auth:
  mode: password
  users: [{name: sam, password_hash: "$2a$12$abcdefghijklmnopqrstuuJ0nqZr9Tf3k8hS9ZcWv1a2b3c4d5e6"}]
webhooks: [{name: ops, url: "${ALERT_HOOK}", format: slack}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Webhooks[0].URL != "https://hooks.slack.com/services/T0/B0/x" {
		t.Errorf("webhook url = %q", c.Webhooks[0].URL)
	}
	if !strings.HasPrefix(c.Auth.Users[0].PasswordHash, "$2a$12$abc") {
		t.Errorf("the bcrypt hash was changed: %q", c.Auth.Users[0].PasswordHash)
	}

	if _, err := Load(write(t, `webhooks: [{name: ops, url: "${NOT_SET_ANYWHERE}"}]`)); err == nil || !strings.Contains(err.Error(), "NOT_SET_ANYWHERE") {
		t.Errorf("an unset variable gave %v, want an error naming it", err)
	}
}

func TestChecks(t *testing.T) {
	for yaml, want := range map[string]string{
		"auth: {mode: password}":                                 "at least one user",
		"auth: {mode: proxy}":                                    "trusted",
		"capture: {mode: full}":                                  "listen.milter",
		"alerts: [{name: x, metric: nope}]":                      "metric",
		"alerts: [{name: x, metric: rejected, notify: [ghost]}]": "no webhook",
		"bogus_key: 1":                                           "bogus_key",
		"auth: {trusted_proxies: [nope]}":                        "auth.trusted_proxies",
	} {
		if _, err := Load(write(t, yaml)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want it to mention %q", yaml, err, want)
		}
	}
	c, err := Load(write(t, "auth: {mode: proxy, trusted_proxies: [172.64.0.0/13], proxy: {user_header: Remote-User, trusted: [10.1.0.2]}}"))
	if err != nil || len(c.Auth.TrustedPrefixes) != 1 || len(c.Auth.ForwardedPrefixes) != 2 {
		t.Errorf("proxy.trusted is trusted for names and addresses, trusted_proxies for addresses only: %v %v %v", c.Auth.TrustedPrefixes, c.Auth.ForwardedPrefixes, err)
	}
	if c, err := Load(""); err != nil || c.Database.URL != "sqlite:mailinois.db" || c.Auth.Mode != "off" {
		t.Errorf("defaults: %+v %v", c, err)
	}
}

func TestExactSenderSites(t *testing.T) {
	c, err := Load(write(t, "sites: [{name: Status, senders: [' STATUS@widgets.example ']}]"))
	if err != nil || c.Sites[0].Senders[0] != "status@widgets.example" || strings.Join(c.Sites[0].DNSDomains(), ",") != "widgets.example" {
		t.Fatalf("sender site = %+v, %v", c, err)
	}
	for _, sender := range []string{"not-an-address", "Status <status@widgets.example>", "*@widgets.example"} {
		_, err := Load(write(t, "sites: [{name: Status, senders: ['"+sender+"']}]"))
		if err == nil {
			t.Errorf("accepted sender %q", sender)
		}
	}
}

func TestOIDCChecks(t *testing.T) {
	base := "auth: {mode: oidc, oidc: {issuer: https://accounts.example.com, client_id: mailinois, redirect_url: https://mail.example.com/auth/callback"
	for yaml, want := range map[string]string{
		base + "}}": "anyone with an account",
		"auth: {mode: oidc, oidc: {issuer: https://accounts.example.com, allowed_domains: [example.com]}}":                                                     "needs issuer, client_id and redirect_url",
		"auth: {mode: oidc, oidc: {issuer: https://a.example, client_id: m, redirect_url: https://mail.example.com/callback, allowed_domains: [example.com]}}": "ending in /auth/callback",
		base + ", allowed_domains: [example.com], scopes: [email]}}":                                                                                           "must include openid",
		"sites: [{name: Shop}]\n" + base + ", allowed_groups: [mail], group_sites: {mail: [nope]}}}":                                                           "no site \"nope\"",
		"auth: {mode: magic}": "off, password, proxy or oidc",
	} {
		if _, err := Load(write(t, yaml)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want it to mention %q", yaml, err, want)
		}
	}
	for _, yaml := range []string{
		base + ", allowed_domains: [example.com]}}",
		base + ", allowed_groups: [mail-admins]}}",
		base + "}, users: [{name: sam@example.com}]}",
		"sites: [{name: Shop}]\n" + base + ", allowed_groups: [mail], group_sites: {mail: [shop]}}}",
	} {
		c, err := Load(write(t, yaml))
		if err != nil {
			t.Errorf("%s: %v, want it to load", yaml, err)
			continue
		}
		if o := c.Auth.OIDC; o.GroupsClaim != "groups" || o.ButtonLabel != "Sign in with SSO" || len(o.Scopes) != 3 {
			t.Errorf("%s: defaults not applied: %+v", yaml, o)
		}
	}
	// MAILINOIS_PASSWORD beside OIDC is a break-glass user, and leaves the mode alone.
	t.Setenv("MAILINOIS_PASSWORD", "break glass")
	c, err := Load(write(t, base+", allowed_domains: [example.com]}}"))
	if err != nil || c.Auth.Mode != "oidc" || len(c.Auth.Users) != 1 || c.Auth.Users[0].PasswordHash == "" {
		t.Errorf("MAILINOIS_PASSWORD with OIDC: mode %q, users %v, err %v", c.Auth.Mode, c.Auth.Users, err)
	}
}

// The example people copy loads as it is, and the docs page carries it word for word.
func TestExampleConfig(t *testing.T) {
	const example = "../../deploy/config/mailinois.example.yaml"
	if _, err := Load(example); err != nil {
		t.Fatalf("%s: %v", example, err)
	}
	if _, err := Load(write(t, "listen: {htp: \":7480\"}")); err == nil {
		t.Error("a misspelt key loaded, so this test can't catch a broken example")
	}

	want, err := os.ReadFile(example)
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile("../../site/src/content/docs/docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	_, copied, _ := strings.Cut(string(page), "```yaml title=\"mailinois.example.yaml\"\n")
	copied, _, _ = strings.Cut(copied, "\n```")
	if copied+"\n" != string(want) {
		t.Error("the example in docs/configuration.md differs from deploy/config/mailinois.example.yaml")
	}
}
