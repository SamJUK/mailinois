// Package report turns log lines already in a store into a one-off report: the overview, why mail waited or
// failed, and each message's story, as one HTML file that works offline, Markdown, or JSON.
package report

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/template"
	"time"

	"github.com/SamJUK/mailinois/internal/store"
)

// Source is one file (or stdin) the report read.
type Source struct {
	Name     string `json:"name"`
	Lines    int    `json:"lines"`
	Unparsed int    `json:"unparsed"`
}

type Report struct {
	Version     string                `json:"version"`
	GeneratedAt time.Time             `json:"generated_at"`
	Timezone    string                `json:"timezone"`
	Sources     []Source              `json:"sources"`
	Search      string                `json:"search,omitempty"`
	Since       time.Time             `json:"since"`
	Until       time.Time             `json:"until"`
	Summary     store.Dashboard       `json:"summary"`
	Providers   []store.ProviderStats `json:"providers"`
	Reasons     []store.Reason        `json:"deferral_reasons"`
	Queue       []store.QueuedMessage `json:"still_queued"`
	Messages    []*store.Message      `json:"messages"`
	More        bool                  `json:"more"` // more messages matched than are listed
}

// Build reads the report for f out of st: every count over the whole period, and up to limit messages in full.
func Build(ctx context.Context, st *store.Store, f store.Filter, limit int) (*Report, error) {
	// The store hands times back in time.Local, the log's zone; the period comes from the caller, so match it.
	r := &Report{Since: f.Since.In(time.Local), Until: f.Until.In(time.Local), GeneratedAt: time.Now()}
	step := time.Hour
	if span := f.Until.Sub(f.Since); span > 48*time.Hour {
		step = (span / 48).Truncate(time.Hour)
	}
	// The log is all there is: a message without its removal line was still queued when the log ended.
	forever := 100 * 365 * 24 * time.Hour
	var err error
	if r.Summary, err = st.Dashboard(ctx, f, step, forever); err != nil {
		return nil, err
	}
	if r.Providers, err = st.Providers(ctx, f); err != nil {
		return nil, err
	}
	if r.Reasons, err = st.DeferralReasons(ctx, f); err != nil {
		return nil, err
	}
	if r.Queue, err = st.Queue(ctx, f, forever, limit); err != nil {
		return nil, err
	}
	rows, err := st.Messages(ctx, f, 0, limit+1)
	if err != nil {
		return nil, err
	}
	if len(rows) > limit {
		rows, r.More = rows[:limit], true
	}
	for _, row := range rows {
		m, err := st.Message(ctx, f, row.ID)
		if err != nil {
			return nil, err
		}
		r.Messages = append(r.Messages, m)
	}
	return r, nil
}

func (r *Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

var (
	//go:embed report.css
	css string
	//go:embed report.js
	js string
	//go:embed page.html
	pageHTML string
	page     = template.Must(template.New("page").Parse(pageHTML))
)

// HTML writes one file that needs nothing else: styles, script and data inline. Untrusted text (addresses,
// remote servers' answers, raw log lines) travels only inside the JSON, which json.Marshal escapes for a
// script element, and the script puts it on the page as text. The CSP lets only this script and these styles
// run, and nothing load from anywhere.
func (r *Report) HTML(w io.Writer) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	hash := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	return page.Execute(w, map[string]string{
		"CSP":  "default-src 'none'; script-src " + hash(js) + "; style-src " + hash(css) + "; img-src data:; base-uri 'none'; form-action 'none'",
		"CSS":  css,
		"JS":   js,
		"Data": string(data),
	})
}

// Markdown writes the same report for a ticket or a chat. Untrusted text goes in code spans and fenced blocks,
// where Markdown renders no links or images.
func (r *Report) Markdown(w io.Writer) error {
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }
	when := func(t time.Time) string { return t.Format("2006-01-02 15:04:05") }
	d := r.Summary

	p("# Mailinois report\n\n")
	p("- **Period:** %s to %s (%s)\n", when(r.Since), when(r.Until), r.Timezone)
	if r.Search != "" {
		p("- **Search:** %s\n", code(r.Search))
	}
	for _, s := range r.Sources {
		p("- **Read:** %s, %d lines (%d not syslog)\n", code(s.Name), s.Lines, s.Unparsed)
	}
	p("- **Made:** %s by Mailinois %s\n\n", when(r.GeneratedAt), r.Version)

	p("## Summary\n\n| Received | Delivered | Deferrals | Bounced | Expired | Rejected | Still queued |\n|---|---|---|---|---|---|---|\n")
	p("| %d | %d | %d | %d | %d | %d | %d |\n\n", d.Received, d.Sent, d.Deferrals, d.Bounced, d.Expired, d.Rejected, d.Queue)

	if len(r.Reasons) > 0 {
		p("## Why mail waited\n\n| Provider | Code | Deferrals | Latest answer |\n|---|---|---|---|\n")
		for _, x := range r.Reasons {
			p("| %s | %s | %d | %s |\n", x.Provider, code(x.DSN), x.Count, code(x.Example))
		}
		p("\n")
	}
	if len(r.Providers) > 0 {
		p("## Providers\n\n| Provider | Attempts | Delivered | Deferred | Failed |\n|---|---|---|---|---|\n")
		for _, x := range r.Providers {
			if x.Attempts > 0 {
				p("| %s | %d | %d | %d | %d |\n", x.Provider, x.Attempts, x.Sent, x.Deferred, x.Failed)
			}
		}
		p("\n")
	}
	if len(r.Queue) > 0 {
		p("## Still queued when the log ends\n\n| Received | Server | Queue ID | Sender | Attempts | Latest answer |\n|---|---|---|---|---|---|\n")
		for _, x := range r.Queue {
			p("| %s | %s | %s | %s | %d | %s |\n", when(x.ReceivedAt), code(x.Server), code(x.QueueID), code(x.Sender), x.Attempts, code(x.Response))
		}
		p("\n")
	}

	p("## Messages\n\n")
	if r.More {
		p("The first %d matching messages, newest first. Narrow the search, or raise `-limit`, for the rest.\n\n", len(r.Messages))
	}
	for _, m := range r.Messages {
		p("### %s, %s, %s\n\n", when(m.ReceivedAt), m.QueueID, m.Status)
		p("- **Server:** %s\n- **From:** %s\n", code(m.Server), code(m.Sender))
		if m.MessageID != "" {
			p("- **Message-ID:** %s\n", code(m.MessageID))
		}
		if m.ClientHost != "" || m.ClientIP != "" {
			p("- **Client:** %s\n", code(strings.TrimSpace(m.ClientHost+" "+m.ClientIP)))
		}
		if len(m.Recipients) > 0 {
			p("\n| Recipient | Status | Code | Attempts | Answer |\n|---|---|---|---|---|\n")
			for _, rc := range m.Recipients {
				p("| %s | %s | %s | %d | %s |\n", code(rc.Address), rc.Status, code(rc.DSN), rc.Attempts, code(rc.Response))
			}
		}
		if len(m.Events) > 0 {
			lines := make([]string, len(m.Events))
			for i, e := range m.Events {
				lines[i] = when(e.At) + " " + e.Program + ": " + e.Raw
			}
			fence := fenceFor(lines)
			p("\n%stext\n%s\n%s\n", fence, strings.Join(lines, "\n"), fence)
		}
		p("\n")
	}
	return nil
}

// code makes s a code span that survives a table: backticks can't close it, pipes and newlines can't split it.
func code(s string) string {
	if s == "" {
		return ""
	}
	s = strings.NewReplacer("|", `\|`, "\r", " ", "\n", " ").Replace(s)
	ticks := strings.Repeat("`", longestRun(s, '`')+1)
	return ticks + " " + s + " " + ticks
}

// fenceFor is a code fence longer than any run of backticks in lines.
func fenceFor(lines []string) string {
	n := 3
	for _, l := range lines {
		n = max(n, longestRun(l, '`')+1)
	}
	return strings.Repeat("`", n)
}

func longestRun(s string, c byte) int {
	best, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			run++
			best = max(best, run)
		} else {
			run = 0
		}
	}
	return best
}
