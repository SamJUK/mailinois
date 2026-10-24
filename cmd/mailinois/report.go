package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SamJUK/mailinois/internal/report"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/syslog"
)

// runReport reads mail logs into a throwaway database and writes one report file, with no server and no config.
func runReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	search := fs.String("q", "", `only messages matching this search, as in the dashboard: "to:sam@example.com after:6h"`)
	out := fs.String("o", "", "the file to write; .html, .md or .json picks the format (default mailinois-report-<time>.html; - for stdout)")
	format := fs.String("format", "", "html, md or json, when -o doesn't say")
	server := fs.String("server", "", "server name for lines without one, such as docker logs output")
	tz := fs.String("tz", "", "time zone of timestamps that carry none, as in /var/log/mail.log (default this machine's)")
	limit := fs.Int("limit", 1000, "messages listed in full; every count still covers the whole log")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: mailinois report [flags] [file...]\n\nReads mail logs (plain or .gz, or stdin with -) and writes one report: the summary, why mail\nwaited or failed, and each message's story. Nothing runs afterwards and nothing is kept.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	loc := time.Local
	if *tz != "" {
		var err error
		if loc, err = time.LoadLocation(*tz); err != nil {
			return fmt.Errorf("-tz: %w", err)
		}
	}
	// One-shot process: the store hands times back in time.Local, so make that the log's zone for the whole report.
	time.Local = loc

	if *format == "" {
		switch ext := strings.ToLower(filepath.Ext(*out)); ext {
		case ".md", ".markdown":
			*format = "md"
		case ".json":
			*format = "json"
		case "", ".html", ".htm":
			*format = "html"
		default:
			return fmt.Errorf("-o: can't tell the format from %q; use .html, .md or .json (for a PDF, open the HTML report and print it)", ext)
		}
	}
	if *format != "html" && *format != "md" && *format != "json" {
		return fmt.Errorf("-format: %q is not html, md or json (for a PDF, open the HTML report and print it)", *format)
	}
	if *out == "" {
		*out = "mailinois-report-" + time.Now().Format("20060102-150405") + "." + *format
	}

	dir, err := os.MkdirTemp("", "mailinois-report-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	st, err := store.Open(ctx, "sqlite:"+filepath.Join(dir, "report.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"-"}
	}
	var sources []report.Source
	var first, last time.Time
	for _, path := range paths {
		lines, unparsed, err := readLog(path, loc)
		if err != nil {
			return err
		}
		for _, l := range lines {
			if first.IsZero() || l.Time.Before(first) {
				first = l.Time
			}
			if l.Time.After(last) {
				last = l.Time
			}
		}
		if err := ingestLines(ctx, st, lines, *server); err != nil {
			return err
		}
		sources = append(sources, report.Source{Name: path, Lines: len(lines) + unparsed, Unparsed: unparsed})
	}
	if first.IsZero() {
		return fmt.Errorf("no mail log lines in %s", strings.Join(paths, ", "))
	}

	f := store.Filter{Since: first, Until: last.Add(time.Microsecond)}
	q := store.ParseSearch(*search, time.Now())
	f.Terms = q.Terms
	if q.After != nil {
		f.Since = *q.After
	}
	if q.Before != nil {
		f.Until = *q.Before
	}
	r, err := report.Build(ctx, st, f, *limit)
	if err != nil {
		return err
	}
	r.Version, r.Sources, r.Search, r.Timezone = version, sources, *search, zoneName(loc)

	w := io.Writer(os.Stdout)
	if *out != "-" {
		file, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer file.Close()
		w = file
	}
	switch *format {
	case "md":
		err = r.Markdown(w)
	case "json":
		err = r.JSON(w)
	default:
		err = r.HTML(w)
	}
	if err != nil {
		return err
	}
	d := r.Summary
	fmt.Fprintf(os.Stderr, "%s: %d messages (%d delivered, %d deferrals, %d bounced, %d expired, %d rejected, %d still queued)\n",
		*out, d.Received, d.Sent, d.Deferrals, d.Bounced, d.Expired, d.Rejected, d.Queue)
	return nil
}

// readLog parses one log file, gzipped or not, or stdin for "-".
func readLog(path string, loc *time.Location) (lines []syslog.Line, unparsed int, err error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, err
		}
		defer f.Close()
		r = f
	}
	br := bufio.NewReader(r)
	if magic, _ := br.Peek(2); bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", path, err)
		}
		defer zr.Close()
		br = bufio.NewReader(zr)
	}
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	now := time.Now()
	for sc.Scan() {
		if l, ok := syslog.Parse(sc.Text(), loc, now); ok {
			lines = append(lines, l)
		} else if strings.TrimSpace(sc.Text()) != "" {
			unparsed++
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("%s: %w", path, err)
	}
	return lines, unparsed, nil
}

// zoneName names loc for the report: its IANA name, or for the machine's own zone, its abbreviation and offset.
func zoneName(loc *time.Location) string {
	if name := loc.String(); name != "Local" {
		return name
	}
	return time.Now().In(loc).Format("MST, UTC-07:00")
}
