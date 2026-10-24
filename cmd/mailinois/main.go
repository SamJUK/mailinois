// Mailinois watches Postfix: it reads mail logs over syslog, follows every message by queue ID, and serves
// a dashboard of deliveries, deferrals, bounces, queues, providers, blocklists and DNS.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"golang.org/x/crypto/bcrypt"

	"github.com/SamJUK/mailinois/internal/agent"
	"github.com/SamJUK/mailinois/internal/alerts"
	"github.com/SamJUK/mailinois/internal/auth"
	"github.com/SamJUK/mailinois/internal/capture"
	"github.com/SamJUK/mailinois/internal/config"
	"github.com/SamJUK/mailinois/internal/demo"
	"github.com/SamJUK/mailinois/internal/dnsbl"
	"github.com/SamJUK/mailinois/internal/dnscheck"
	"github.com/SamJUK/mailinois/internal/ingest"
	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/syslog"
	"github.com/SamJUK/mailinois/internal/web"
)

var version = "dev"

const usage = `Mailinois %s: Postfix observability.

Usage:
  mailinois serve [-config mailinois.yaml]      ingest syslog, capture, check, alert and serve the dashboard (the default)
  mailinois import [-config ...] [file...]      read mail logs from files or stdin (-), then exit
  mailinois report [-q search] [-o file] [file...]  one report file (HTML, Markdown or JSON) from mail logs, no server
  mailinois agent -server URL -token TOKEN ...  ship a mail server's logs and captures to a Mailinois server
  mailinois hash-password                        read a password on stdin, print its hash for auth.users
  mailinois token                                print a new API token and the hash for auth.tokens
  mailinois check [-config ...]                  load the config (and the environment it names), report problems, exit
  mailinois healthcheck [-url ...]               exit 0 when the dashboard answers /healthz (for container health checks)
  mailinois version

The config file is optional; MAILINOIS_CONFIG names it too. See deploy/config/mailinois.example.yaml.
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, args)
	case "import":
		err = importLogs(ctx, args)
	case "report":
		err = runReport(ctx, args)
	case "agent":
		err = runAgent(ctx, args)
	case "hash-password":
		err = hashPassword()
	case "token":
		token, hash := auth.NewToken()
		fmt.Printf("Token (give this to the client; it isn't stored anywhere): %s\nFor auth.tokens in the config:        hash: %q\n", token, hash)
	case "check":
		err = checkConfig(args)
	case "healthcheck":
		err = healthcheck(args)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	if err != nil {
		slog.Error(cmd, "err", err)
		os.Exit(1)
	}
}

func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, error) {
	path := fs.String("config", os.Getenv("MAILINOIS_CONFIG"), "path to mailinois.yaml (MAILINOIS_CONFIG)")
	fs.Parse(args)
	return config.Load(*path)
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer st.Close()

	secret := cfg.Auth.SessionSecret
	if secret == "" {
		if secret, err = st.Setting(ctx, "session_secret", auth.NewSecret); err != nil {
			return err
		}
	}

	p := ingest.New(st, cfg.Location, 0)
	go p.Run(ctx)
	if addr := cfg.Listen.Syslog; addr != "" {
		go listen("syslog tcp", addr, func() error { return syslog.ListenTCP(ctx, addr, p.Frame) })
		go listen("syslog udp", addr, func() error { return syslog.ListenUDP(ctx, addr, p.Frame) })
	}
	if addr := cfg.Listen.Milter; addr != "" && cfg.Capture.Mode != "off" {
		go listen("milter", addr, func() error {
			return capture.Serve(ctx, addr, capture.Mode(cfg.Capture.Mode), cfg.Capture.MaxSize, func(m capture.Message) { p.Capture(fromCapture(m)) })
		})
	}

	if cfg.Demo {
		if err := startDemo(ctx, st, p, cfg); err != nil {
			return err
		}
	} else {
		go runChecks(ctx, st, cfg)
	}
	go purge(ctx, st, cfg)
	evaluator := &alerts.Evaluator{Store: st, Config: cfg, Lifetime: cfg.QueueLifetime.D(), URL: os.Getenv("MAILINOIS_URL")}
	if len(cfg.Alerts) > 0 {
		go evaluator.Run(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.Listen.HTTP,
		Handler:           (&web.Server{Store: st, Config: cfg, Auth: auth.New(cfg.Auth, secret), Pipeline: p, Alerts: evaluator, Version: version}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	slog.Info("mailinois", "version", version, "database", st.Dialect(), "http", cfg.Listen.HTTP, "syslog", cfg.Listen.Syslog,
		"milter", cfg.Listen.Milter, "capture", cfg.Capture.Mode, "auth", cfg.Auth.Mode, "sites", len(cfg.Sites), "alerts", len(cfg.Alerts), "demo", cfg.Demo)
	if cfg.Auth.Mode == "off" {
		slog.Warn("auth is off: the dashboard and API are open to anyone who can reach " + cfg.Listen.HTTP)
	}
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func fromCapture(m capture.Message) store.Capture {
	return store.Capture{QueueID: m.QueueID, MessageID: m.MessageID, Server: m.Server, At: m.At, Header: m.Header, Body: m.Body, Size: m.Size, Truncated: m.Truncated}
}

func listen(name, addr string, run func() error) {
	slog.Info("listening", "for", name, "addr", addr)
	if err := run(); err != nil {
		slog.Error("listener stopped", "for", name, "err", err)
	}
}

func purge(ctx context.Context, st *store.Store, cfg *config.Config) {
	for {
		now := time.Now()
		if n, err := st.Purge(ctx, now.Add(-cfg.Retention.Messages.D()), now.Add(-cfg.Retention.Captures.D())); err != nil {
			slog.Error("purge", "err", err)
		} else if n > 0 {
			slog.Info("purged old messages", "count", n)
		}
		if err := st.Optimize(ctx); err != nil {
			slog.Error("optimize", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// runChecks looks the sending IPs up in the blocklists and checks every sending domain's SPF, DKIM and
// DMARC, at start and every checks.interval.
func runChecks(ctx context.Context, st *store.Store, cfg *config.Config) {
	resolver := dnsbl.Resolver(cfg.Checks.DNSServer)
	blocklists := &dnsbl.Checker{Store: st, IPs: cfg.Checks.SendingIPs, Zones: cfg.Checks.DNSBLZones, Resolver: resolver}
	records := &dnscheck.Checker{Resolver: resolver}
	for {
		if len(blocklists.IPs) > 0 {
			blocklists.CheckAll(ctx)
		}
		selectors, _ := st.Selectors(ctx, time.Now().Add(-30*24*time.Hour))
		for _, domain := range sendingDomains(ctx, st, cfg) {
			results := records.SPF(ctx, domain, cfg.Checks.SendingIPs)
			for _, sel := range union(selectors[domain], cfg.Checks.DKIMSelectors[domain]) {
				results = append(results, records.DKIM(ctx, domain, sel))
			}
			results = append(results, records.DMARC(ctx, domain))
			for _, r := range results {
				err := st.SaveDNS(ctx, store.DNSResult{Domain: r.Domain, Kind: r.Kind, Subject: r.Subject, Status: r.Status, Summary: r.Summary, Record: r.Record, CheckedAt: time.Now()})
				if err != nil {
					slog.Error("save dns check", "domain", domain, "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(cfg.Checks.Interval.D(), time.Hour)):
		}
	}
}

// sendingDomains are the configured ones, the sites', and the busiest seen sending in the last week.
func sendingDomains(ctx context.Context, st *store.Store, cfg *config.Config) []string {
	domains := slices.Clone(cfg.Checks.Domains)
	for _, s := range cfg.Sites {
		domains = append(domains, s.DNSDomains()...)
	}
	seen, _ := st.SenderDomains(ctx, time.Now().Add(-7*24*time.Hour), 20)
	return union(domains, seen)
}

func union(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		for _, v := range l {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

func startDemo(ctx context.Context, st *store.Store, p *ingest.Pipeline, cfg *config.Config) error {
	if len(cfg.Checks.SendingIPs) == 0 {
		cfg.Checks.SendingIPs = demo.IPs()
	}
	empty, err := st.Empty(ctx)
	if err != nil {
		return err
	}
	if empty {
		const days = 14
		now := time.Now()
		if err := demo.SeedBlocklists(ctx, st, days, cfg.Checks.DNSBLZones, now); err != nil {
			return err
		}
		if err := demo.SeedDNS(ctx, st, now); err != nil {
			return err
		}
		go func() {
			lines := demo.History(days, now)
			slog.Info("demo: loading made-up history", "days", days, "lines", len(lines))
			if err := ingestLines(ctx, st, lines, ""); err != nil {
				slog.Error("demo history", "err", err)
				return
			}
			slog.Info("demo: history loaded")
		}()
	}
	go demo.Live(ctx, p.Line, p.Capture)
	return nil
}

func ingestLines(ctx context.Context, st *store.Store, lines []syslog.Line, server string) error {
	batch := make([]postfix.Event, 0, 1000)
	flush := func() error {
		_, err := st.Ingest(ctx, batch)
		batch = batch[:0]
		return err
	}
	for _, l := range lines {
		if server != "" {
			l.Host = server
		}
		if e, ok := postfix.Parse(l); ok {
			if batch = append(batch, e); len(batch) == cap(batch) {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

func importLogs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	server := fs.String("server", "", "server name for every line, e.g. for docker logs output, which has no host field")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: mailinois import [flags] [file...]\n\nReads syslog files (or stdin with -) in any format serve accepts. Lines already stored are skipped.\n\n")
		fs.PrintDefaults()
	}
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer st.Close()

	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"-"}
	}
	for _, path := range paths {
		var r io.Reader = os.Stdin
		if path != "-" {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			r = f
		}
		var lines []syslog.Line
		skipped := 0
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if l, ok := syslog.Parse(sc.Text(), cfg.Location, time.Now()); ok {
				lines = append(lines, l)
			} else {
				skipped++
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
		if err := ingestLines(ctx, st, lines, *server); err != nil {
			return err
		}
		fmt.Printf("%s: read %d lines (%d were not syslog)\n", path, len(lines), skipped)
	}
	return nil
}

func runAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	var files multi
	cfg := agent.Config{}
	fs.StringVar(&cfg.URL, "server", os.Getenv("MAILINOIS_SERVER"), "Mailinois server URL (MAILINOIS_SERVER)")
	token := fs.String("token", os.Getenv("MAILINOIS_TOKEN"), "API token with the ingest scope (MAILINOIS_TOKEN); -token-file reads it from a file")
	tokenFile := fs.String("token-file", "", "file holding the token")
	fs.StringVar(&cfg.ServerName, "name", "", "server name for lines without a host field (docker logs)")
	fs.Var(&files, "file", "log file to follow; repeat for more")
	fs.StringVar(&cfg.SyslogListen, "syslog", "", "take syslog locally on this address, e.g. 127.0.0.1:5514")
	fs.StringVar(&cfg.MilterListen, "milter", "", "run the capture milter on this address, e.g. 127.0.0.1:8892")
	mode := fs.String("capture", "headers", "capture mode for the milter: headers or full")
	fs.Int64Var(&cfg.MaxSize, "max-size", 1<<20, "bytes kept per captured message")
	fs.StringVar(&cfg.SpoolDir, "spool", "/var/lib/mailinois-agent", "where batches wait while the server is unreachable")
	fs.Parse(args)
	cfg.Token, cfg.Files, cfg.CaptureMode = *token, files, capture.Mode(*mode)
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		cfg.Token = strings.TrimSpace(string(b))
	}
	if cfg.URL == "" || cfg.Token == "" || (len(cfg.Files) == 0 && cfg.SyslogListen == "" && cfg.MilterListen == "") {
		fs.Usage()
		return errors.New("needs -server, -token, and at least one of -file, -syslog or -milter")
	}
	return agent.Run(ctx, cfg)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func hashPassword() error {
	fmt.Fprint(os.Stderr, "Password (it will show as you type; pipe it in to avoid that): ")
	password, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	password = strings.TrimRight(password, "\r\n")
	if len(password) < 12 {
		return errors.New("use at least 12 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr)
	fmt.Println(string(hash))
	return nil
}

// healthcheck is for images without a shell or curl: it exits non-zero unless /healthz answers 200.
func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:7480/healthz", "the health endpoint")
	fs.Parse(args)
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(*url)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", *url, res.Status)
	}
	return nil
}

// checkConfig is for before a deploy: the config parses, its ${NAMES} are set, and it makes sense.
func checkConfig(args []string) error {
	cfg, err := loadConfig(flag.NewFlagSet("check", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	fmt.Printf("ok: database %s; auth %s with %d users and %d tokens; listening http %q, syslog %q, milter %q; capture %s; %d sites, %d sending IPs, %d webhooks, %d alerts\n",
		strings.SplitN(cfg.Database.URL, ":", 2)[0], cfg.Auth.Mode, len(cfg.Auth.Users), len(cfg.Auth.Tokens), cfg.Listen.HTTP, cfg.Listen.Syslog,
		cfg.Listen.Milter, cfg.Capture.Mode, len(cfg.Sites), len(cfg.Checks.SendingIPs), len(cfg.Webhooks), len(cfg.Alerts))
	return nil
}
