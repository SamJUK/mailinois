// Package config reads mailinois.yaml and the environment variables that override it.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"
)

type Config struct {
	Listen struct {
		HTTP   string `yaml:"http"`
		Syslog string `yaml:"syslog"`
		Milter string `yaml:"milter"`
	} `yaml:"listen"`
	Database struct {
		URL string `yaml:"url"`
	} `yaml:"database"`
	Retention struct {
		Messages Duration `yaml:"messages"`
		Captures Duration `yaml:"captures"`
	} `yaml:"retention"`
	QueueLifetime Duration `yaml:"queue_lifetime"`
	LogTimezone   string   `yaml:"log_timezone"`
	Auth          Auth     `yaml:"auth"`
	Sites         []Site   `yaml:"sites"`
	Capture       struct {
		Mode    string `yaml:"mode"`
		MaxSize int64  `yaml:"max_size"`
	} `yaml:"capture"`
	Checks struct {
		SendingIPs    []string            `yaml:"sending_ips"`
		DNSBLZones    []string            `yaml:"dnsbl_zones"`
		Interval      Duration            `yaml:"interval"`
		DNSServer     string              `yaml:"dns_server"`
		Domains       []string            `yaml:"domains"`
		DKIMSelectors map[string][]string `yaml:"dkim_selectors"`
	} `yaml:"checks"`
	Webhooks []Webhook `yaml:"webhooks"`
	Alerts   []Alert   `yaml:"alerts"`
	Demo     bool      `yaml:"demo"`

	Location *time.Location `yaml:"-"`
}

type Auth struct {
	Mode          string `yaml:"mode"` // off, password, proxy, oidc
	SessionSecret string `yaml:"session_secret"`
	Users         []User `yaml:"users"`
	Proxy         struct {
		UserHeader string   `yaml:"user_header"`
		Trusted    []string `yaml:"trusted"`
	} `yaml:"proxy"`
	Tokens []Token `yaml:"tokens"`
	OIDC   OIDC    `yaml:"oidc"`
	// TrustedProxies are where X-Forwarded-For is believed, in any mode (proxy.trusted counts too).
	TrustedProxies []string `yaml:"trusted_proxies"`

	TrustedPrefixes   []netip.Prefix `yaml:"-"` // proxy.trusted
	ForwardedPrefixes []netip.Prefix `yaml:"-"` // trusted_proxies and proxy.trusted
}

// OIDC is sign-in through an OpenID Connect provider: Google Workspace, Entra ID, Okta, Keycloak, Authentik.
type OIDC struct {
	Issuer         string              `yaml:"issuer"`
	ClientID       string              `yaml:"client_id"`
	ClientSecret   string              `yaml:"client_secret"`
	RedirectURL    string              `yaml:"redirect_url"` // https://<mailinois>/auth/callback, as registered with the provider
	Scopes         []string            `yaml:"scopes"`
	AllowedDomains []string            `yaml:"allowed_domains"` // a verified email in one of these
	AllowedGroups  []string            `yaml:"allowed_groups"`  // a member of any one of these
	GroupsClaim    string              `yaml:"groups_claim"`
	GroupSites     map[string][]string `yaml:"group_sites"` // group to the site slugs its members see
	ButtonLabel    string              `yaml:"button_label"`
}

type User struct {
	Name         string   `yaml:"name"`
	PasswordHash string   `yaml:"password_hash"`
	Sites        []string `yaml:"sites"`
}

type Token struct {
	Name   string   `yaml:"name"`
	Hash   string   `yaml:"hash"`
	Scopes []string `yaml:"scopes"`
	Sites  []string `yaml:"sites"`
}

type Site struct {
	Name    string   `yaml:"name"`
	Slug    string   `yaml:"slug"`
	Domains []string `yaml:"domains"`
	Senders []string `yaml:"senders"`
	Servers []string `yaml:"servers"`
}

// DNSDomains includes the domains of exact senders that share a relay with another app.
func (s Site) DNSDomains() []string {
	domains := slices.Clone(s.Domains)
	for _, sender := range s.Senders {
		_, domain, _ := strings.Cut(sender, "@")
		if domain != "" && !slices.Contains(domains, domain) {
			domains = append(domains, domain)
		}
	}
	return domains
}

type Webhook struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	Format string `yaml:"format"` // json, slack, discord
	Secret string `yaml:"secret"`
}

type Alert struct {
	Name        string    `yaml:"name"`
	Metric      string    `yaml:"metric"`
	Above       Threshold `yaml:"above"`
	Window      Duration  `yaml:"window"`
	MinMessages int       `yaml:"min_messages"`
	Site        string    `yaml:"site"`
	Provider    string    `yaml:"provider"`
	Server      string    `yaml:"server"`
	Notify      []string  `yaml:"notify"`
	Repeat      Duration  `yaml:"repeat"`
}

var Metrics = []string{"bounce_rate", "deferral_rate", "rejected", "queue_depth", "queue_oldest", "blocklisted", "server_silent", "dns_failing"}

var DefaultZones = []string{"zen.spamhaus.org", "b.barracudacentral.org", "bl.spamcop.net", "psbl.surriel.com", "bl.mailspike.net"}

type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration like 90s, 5m or 6h", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Threshold is a number, or a duration in seconds for the time-based metrics.
type Threshold float64

func (t *Threshold) UnmarshalYAML(n *yaml.Node) error {
	if f, err := strconv.ParseFloat(n.Value, 64); err == nil {
		*t = Threshold(f)
		return nil
	}
	d, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is neither a number nor a duration", n.Line, n.Value)
	}
	*t = Threshold(d.Seconds())
	return nil
}

func defaults() *Config {
	c := &Config{}
	c.Listen.HTTP = ":7480"
	c.Listen.Syslog = ":5514"
	c.Database.URL = "sqlite:mailinois.db"
	c.Retention.Messages = Duration(720 * time.Hour)
	c.Retention.Captures = Duration(168 * time.Hour)
	c.QueueLifetime = Duration(120 * time.Hour)
	c.LogTimezone = "UTC"
	c.Auth.Mode = "off"
	c.Auth.Proxy.UserHeader = "Remote-User"
	c.Auth.OIDC.Scopes = []string{"openid", "email", "profile"}
	c.Auth.OIDC.GroupsClaim = "groups"
	c.Auth.OIDC.ButtonLabel = "Sign in with SSO"
	c.Capture.Mode = "off"
	c.Capture.MaxSize = 1 << 20
	c.Checks.DNSBLZones = DefaultZones
	c.Checks.Interval = Duration(6 * time.Hour)
	return c
}

// Load reads path (none: defaults only), applies the environment, and checks the result.
func Load(path string) (*Config, error) {
	c := defaults()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		expanded, err := expand(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(expanded))
		dec.KnownFields(true)
		if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := c.applyEnv(); err != nil {
		return nil, err
	}
	return c, c.check()
}

var envRef = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// expand swaps ${NAME} for the environment variable, so secrets such as webhook URLs can live in an env file
// rather than the config. Only braced upper-case names, so a bcrypt hash's $2a$ is left alone, and never in
// comment lines. A name that isn't set is an error, not an empty string.
func expand(s string) (string, error) {
	var missing []string
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines[i] = envRef.ReplaceAllStringFunc(line, func(ref string) string {
			name := ref[2 : len(ref)-1]
			v, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
	}
	if missing != nil {
		return "", fmt.Errorf("the config uses ${%s}, but it isn't set", strings.Join(missing, "}, ${"))
	}
	return strings.Join(lines, "\n"), nil
}

func (c *Config) applyEnv() error {
	set := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	set("MAILINOIS_DATABASE_URL", &c.Database.URL)
	set("MAILINOIS_HTTP", &c.Listen.HTTP)
	set("MAILINOIS_SYSLOG", &c.Listen.Syslog)
	set("MAILINOIS_MILTER", &c.Listen.Milter)
	set("MAILINOIS_LOG_TZ", &c.LogTimezone)
	set("MAILINOIS_DNS_SERVER", &c.Checks.DNSServer)
	if v := os.Getenv("MAILINOIS_SENDING_IPS"); v != "" {
		c.Checks.SendingIPs = List(v)
	}
	if v := os.Getenv("MAILINOIS_DNSBL_ZONES"); v != "" {
		c.Checks.DNSBLZones = List(v)
	}
	for key, dst := range map[string]*Duration{"MAILINOIS_RETENTION": &c.Retention.Messages, "MAILINOIS_QUEUE_LIFETIME": &c.QueueLifetime, "MAILINOIS_DNSBL_INTERVAL": &c.Checks.Interval} {
		if v := os.Getenv(key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = Duration(d)
		}
	}
	if v := os.Getenv("MAILINOIS_DEMO"); v != "" {
		c.Demo = v != "0" && v != "false"
	}
	// The shortcut from before the config file: one admin user with this password.
	password := os.Getenv("MAILINOIS_PASSWORD")
	if path := os.Getenv("MAILINOIS_PASSWORD_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		password = strings.TrimSpace(string(b))
	}
	if password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if c.Auth.Mode != "oidc" { // with OIDC, it's the break-glass login beside it
			c.Auth.Mode = "password"
		}
		c.Auth.Users = append(c.Auth.Users, User{Name: env("MAILINOIS_USER", "admin"), PasswordHash: string(hash)})
	}
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var slugChars = regexp.MustCompile(`[^a-z0-9]+`)

func (c *Config) check() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	loc, err := time.LoadLocation(c.LogTimezone)
	if err != nil {
		fail("log_timezone: %w", err)
	}
	c.Location = loc

	switch c.Auth.Mode {
	case "off":
	case "password":
		if len(c.Auth.Users) == 0 {
			fail("auth: password mode needs at least one user")
		}
	case "proxy":
		if len(c.Auth.Proxy.Trusted) == 0 {
			fail("auth.proxy.trusted: list the proxy's addresses, or anyone could claim to be anyone")
		}
	case "oidc":
		o := c.Auth.OIDC
		if o.Issuer == "" || o.ClientID == "" || o.RedirectURL == "" {
			fail("auth.oidc: needs issuer, client_id and redirect_url")
		}
		if u, err := url.Parse(o.RedirectURL); o.RedirectURL != "" && (err != nil || u.Host == "" || u.Path != "/auth/callback") {
			fail("auth.oidc.redirect_url: want Mailinois's own address ending in /auth/callback, such as https://mailinois.example.com/auth/callback")
		}
		if len(o.AllowedDomains) == 0 && len(o.AllowedGroups) == 0 && len(c.Auth.Users) == 0 {
			fail("auth.oidc: set allowed_domains, allowed_groups or auth.users, or anyone with an account at the provider could sign in")
		}
		if !slices.Contains(o.Scopes, "openid") {
			fail("auth.oidc.scopes: must include openid")
		}
	default:
		fail("auth.mode: %q is not off, password, proxy or oidc", c.Auth.Mode)
	}
	prefixes := func(field string, list []string) (out []netip.Prefix) {
		for _, t := range list {
			if !strings.Contains(t, "/") {
				t += map[bool]string{true: "/128", false: "/32"}[strings.Contains(t, ":")]
			}
			p, err := netip.ParsePrefix(t)
			if err != nil {
				fail("%s: %w", field, err)
				continue
			}
			out = append(out, p)
		}
		return out
	}
	c.Auth.TrustedPrefixes = prefixes("auth.proxy.trusted", c.Auth.Proxy.Trusted)
	c.Auth.ForwardedPrefixes = append(prefixes("auth.trusted_proxies", c.Auth.TrustedProxies), c.Auth.TrustedPrefixes...)

	slugs := map[string]bool{}
	for i := range c.Sites {
		s := &c.Sites[i]
		if s.Slug == "" {
			s.Slug = strings.Trim(slugChars.ReplaceAllString(strings.ToLower(s.Name), "-"), "-")
		}
		if s.Name == "" {
			s.Name = s.Slug
		}
		if s.Slug == "" || slugs[s.Slug] {
			fail("sites: every site needs a unique name or slug (%q)", s.Slug)
		}
		slugs[s.Slug] = true
		for j, d := range s.Domains {
			s.Domains[j] = strings.ToLower(strings.TrimSpace(d))
		}
		for j, sender := range s.Senders {
			sender = strings.ToLower(strings.TrimSpace(sender))
			address, err := mail.ParseAddress(sender)
			if strings.ContainsAny(sender, "*?") || err != nil || address.Name != "" || address.Address != sender || !strings.Contains(sender, "@") {
				fail("sites[%s].senders: %q must be an email address", s.Slug, sender)
			}
			s.Senders[j] = sender
		}
	}
	knownSites := func(where string, sites []string) {
		for _, s := range sites {
			if !slugs[s] {
				fail("%s: no site %q", where, s)
			}
		}
	}
	for i, u := range c.Auth.Users {
		// With OIDC a user can be an email address and its sites, with no password of its own.
		if u.Name == "" || (u.PasswordHash == "" && c.Auth.Mode != "oidc") {
			fail("auth.users[%d]: needs a name and a password_hash", i)
		}
		knownSites("auth.users "+u.Name, u.Sites)
	}
	for group, sites := range c.Auth.OIDC.GroupSites {
		knownSites("auth.oidc.group_sites "+group, sites)
	}
	for i, t := range c.Auth.Tokens {
		if t.Name == "" || !strings.HasPrefix(t.Hash, "sha256:") {
			fail("auth.tokens[%d]: needs a name and a sha256: hash from `mailinois token`", i)
		}
		for _, s := range t.Scopes {
			if s != "read" && s != "ingest" {
				fail("auth.tokens %s: scope %q is not read or ingest", t.Name, s)
			}
		}
		knownSites("auth.tokens "+t.Name, t.Sites)
	}

	switch c.Capture.Mode {
	case "off", "headers", "full":
	default:
		fail("capture.mode: %q is not off, headers or full", c.Capture.Mode)
	}
	if c.Capture.Mode != "off" && c.Listen.Milter == "" {
		fail("capture.mode is %s but listen.milter is empty: set an address for Postfix to reach", c.Capture.Mode)
	}

	hooks := map[string]bool{}
	for i := range c.Webhooks {
		w := &c.Webhooks[i]
		if w.Name == "" || w.URL == "" {
			fail("webhooks: every webhook needs a name and a url")
		}
		if w.Format == "" {
			w.Format = "json"
		}
		if !slices.Contains([]string{"json", "slack", "discord"}, w.Format) {
			fail("webhooks %s: format %q is not json, slack or discord", w.Name, w.Format)
		}
		hooks[w.Name] = true
	}
	for i := range c.Alerts {
		a := &c.Alerts[i]
		if a.Name == "" || !slices.Contains(Metrics, a.Metric) {
			fail("alerts[%d]: needs a name and a metric, one of %s", i, strings.Join(Metrics, ", "))
		}
		if a.Window == 0 {
			a.Window = Duration(time.Hour)
		}
		if a.Site != "" {
			knownSites("alerts "+a.Name, []string{a.Site})
		}
		for _, n := range a.Notify {
			if !hooks[n] {
				fail("alerts %s: no webhook named %q", a.Name, n)
			}
		}
	}
	return errors.Join(errs...)
}

// Site returns the site with this slug.
func (c *Config) Site(slug string) (Site, bool) {
	for _, s := range c.Sites {
		if s.Slug == slug {
			return s, true
		}
	}
	return Site{}, false
}

func List(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
