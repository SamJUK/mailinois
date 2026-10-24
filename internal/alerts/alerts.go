// Package alerts checks the configured rules every minute and tells webhooks when one starts or stops firing.
package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/SamJUK/mailinois/internal/config"
	"github.com/SamJUK/mailinois/internal/store"
)

type Evaluator struct {
	Store    *store.Store
	Config   *config.Config
	URL      string // where the dashboard is, for links; may be empty
	Client   *http.Client
	Lifetime time.Duration
}

func (e *Evaluator) Run(ctx context.Context) {
	for {
		if err := e.Check(ctx, time.Now()); err != nil {
			slog.Error("alerts", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// Rule is an alert with its current state, for the Alerts page and the API.
type Rule struct {
	config.Alert
	State store.AlertState
}

func (e *Evaluator) Rules(ctx context.Context) ([]Rule, error) {
	states, err := e.Store.AlertStates(ctx)
	var out []Rule
	for _, a := range e.Config.Alerts {
		s, ok := states[a.Name]
		if !ok {
			s = store.AlertState{Name: a.Name, State: "ok"}
		}
		out = append(out, Rule{a, s})
	}
	return out, err
}

// Check works out every rule's value now, and notifies on firing, on resolving, and on each repeat.
func (e *Evaluator) Check(ctx context.Context, now time.Time) error {
	states, err := e.Store.AlertStates(ctx)
	if err != nil {
		return err
	}
	for _, a := range e.Config.Alerts {
		value, ok, err := e.Value(ctx, a, now)
		if err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
		prev, seen := states[a.Name]
		if !seen {
			prev = store.AlertState{Name: a.Name, State: "ok", Since: now}
		}
		if !ok {
			continue // too little mail to judge: leave it as it was
		}
		next := prev
		next.Value = value
		firing := value > float64(a.Above)
		changed := firing != (prev.State == "firing")
		notify := changed && (firing || seen)
		if changed {
			next.State, next.Since = map[bool]string{true: "firing", false: "ok"}[firing], now
		} else if firing && a.Repeat > 0 && prev.NotifiedAt != nil && now.Sub(*prev.NotifiedAt) >= a.Repeat.D() {
			notify = true
		}
		if notify {
			e.notify(ctx, a, next)
			next.NotifiedAt = &now
		}
		if err := e.Store.SaveAlert(ctx, next, changed); err != nil {
			return err
		}
	}
	return nil
}

// Value is a rule's metric now, and whether there was enough mail to judge a rate.
func (e *Evaluator) Value(ctx context.Context, a config.Alert, now time.Time) (float64, bool, error) {
	f := store.Filter{Since: now.Add(-a.Window.D()), Until: now, Server: a.Server}
	if a.Site != "" {
		site, _ := e.Config.Site(a.Site)
		f.Sites = []store.Scope{{Domains: site.Domains, Senders: site.Senders, Servers: site.Servers}}
	}
	switch a.Metric {
	case "bounce_rate", "deferral_rate", "rejected":
		o, err := e.Store.Outcomes(ctx, f, a.Provider)
		if err != nil {
			return 0, false, err
		}
		switch a.Metric {
		case "bounce_rate":
			return ratio(o.Failed, o.Finals), o.Finals > 0 && o.Finals >= a.MinMessages, nil
		case "deferral_rate":
			return ratio(o.Deferrals, o.Attempts), o.Attempts > 0 && o.Attempts >= a.MinMessages, nil
		}
		return float64(o.Rejected), true, nil
	case "queue_depth", "queue_oldest":
		n, oldest, err := e.Store.QueueNow(ctx, f, e.Lifetime)
		if a.Metric == "queue_depth" || oldest == nil {
			return float64(n), err == nil, err
		}
		return now.Sub(*oldest).Seconds(), true, err
	case "blocklisted":
		ips, err := e.Store.SendingIPs(ctx, e.Config.Checks.SendingIPs)
		listed := 0
		for _, ip := range ips {
			for _, c := range ip.Checks {
				if c.Status == "listed" {
					listed++
				}
			}
		}
		return float64(listed), err == nil, err
	case "server_silent":
		last, err := e.Store.LastLine(ctx)
		silent := 0.0
		for name, at := range last {
			// Servers gone for a week are retired, not silent.
			if (a.Server == "" && now.Sub(at) < 7*24*time.Hour) || name == a.Server {
				silent = max(silent, now.Sub(at).Seconds())
			}
		}
		return silent, err == nil, err
	case "dns_failing":
		results, err := e.Store.DNSResults(ctx)
		var domains []string
		if a.Site != "" {
			site, _ := e.Config.Site(a.Site)
			domains = site.DNSDomains()
		}
		failing := 0
		for _, r := range results {
			if (r.Status == "fail" || r.Status == "missing" || r.Status == "error") && (domains == nil || inDomains(r.Domain, domains)) {
				failing++
			}
		}
		return float64(failing), err == nil, err
	}
	return 0, false, fmt.Errorf("unknown metric %q", a.Metric)
}

func inDomains(d string, domains []string) bool {
	for _, x := range domains {
		if d == x || strings.HasSuffix(d, "."+x) {
			return true
		}
	}
	return false
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// Format shows a metric's value the way its threshold is written.
func Format(metric string, v float64) string {
	switch metric {
	case "bounce_rate", "deferral_rate":
		return fmt.Sprintf("%.1f%%", v*100)
	case "queue_oldest", "server_silent":
		return time.Duration(v * float64(time.Second)).Round(time.Second).String()
	}
	return fmt.Sprintf("%.0f", v)
}

type Payload struct {
	Alert     string  `json:"alert"`
	State     string  `json:"state"` // firing, resolved
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Scope     struct {
		Site     string `json:"site"`
		Provider string `json:"provider"`
		Server   string `json:"server"`
	} `json:"scope"`
	Since time.Time `json:"since"`
	URL   string    `json:"url,omitempty"`
}

func (e *Evaluator) notify(ctx context.Context, a config.Alert, s store.AlertState) {
	p := Payload{Alert: a.Name, State: map[string]string{"firing": "firing", "ok": "resolved"}[s.State], Metric: a.Metric,
		Value: s.Value, Threshold: float64(a.Above), Since: s.Since}
	p.Scope.Site, p.Scope.Provider, p.Scope.Server = a.Site, a.Provider, a.Server
	if e.URL != "" {
		p.URL = strings.TrimSuffix(e.URL, "/") + "/alerts"
	}
	for _, name := range a.Notify {
		for _, w := range e.Config.Webhooks {
			if w.Name == name {
				if err := e.send(ctx, w, p); err != nil {
					slog.Error("webhook", "webhook", w.Name, "alert", a.Name, "err", err)
				}
			}
		}
	}
}

func (e *Evaluator) send(ctx context.Context, w config.Webhook, p Payload) error {
	text := fmt.Sprintf("[%s] %s: %s (threshold %s)", strings.ToUpper(p.State), p.Alert, Format(p.Metric, p.Value), Format(p.Metric, p.Threshold))
	if scope := strings.Trim(strings.Join([]string{p.Scope.Site, p.Scope.Provider, p.Scope.Server}, " "), " "); scope != "" {
		text += " · " + strings.Join(strings.Fields(scope), ", ")
	}
	if p.URL != "" {
		text += " " + p.URL
	}
	var body any = p
	switch w.Format {
	case "slack":
		body = map[string]string{"text": text}
	case "discord":
		body = map[string]string{"content": text}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	var last error
	for attempt, wait := 0, time.Second; attempt < 3; attempt, wait = attempt+1, wait*4 {
		req, _ := http.NewRequestWithContext(ctx, "POST", w.URL, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Mailinois")
		if w.Format == "json" && w.Secret != "" {
			m := hmac.New(sha256.New, []byte(w.Secret))
			m.Write(b)
			req.Header.Set("X-Mailinois-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		res, err := client.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode < 300 {
				return nil
			}
			err = fmt.Errorf("answered %s", res.Status)
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return last
}
