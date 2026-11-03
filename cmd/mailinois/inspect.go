package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/tui"
)

// runInspect reads mail logs into a throwaway database and opens an interactive view of them.
func runInspect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	search := fs.String("q", "", `starting search, as in the dashboard: "to:sam@example.com after:6h"`)
	server := fs.String("server", "", "server name for lines without one, such as docker logs output")
	tz := fs.String("tz", "", "time zone of timestamps that carry none, as in /var/log/mail.log (default this machine's)")
	followLog := fs.Bool("follow", true, "keep reading the newest file as it grows")
	queue := fs.String("queue", "auto", "postqueue -j for the Queue tab: auto (when the log is from this machine), on or off")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: mailinois inspect [flags] [path...]

An interactive view of a node's mail logs: the overview, every message's story, Postfix's queue and new
deliveries as they happen. Read-only, and nothing is kept once you quit.

A path is a file (plain or .gz), a directory (its mail.log or maillog and their rotated copies, oldest
first), a glob in quotes, or - for stdin. With none, /var/log.

`)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *queue != "auto" && *queue != "on" && *queue != "off" {
		return fmt.Errorf("-queue: %q is not auto, on or off", *queue)
	}
	if !term.IsTerminal(os.Stdout.Fd()) {
		return errors.New("mailinois inspect needs a terminal; for a file, use mailinois report")
	}

	loc := time.Local
	if *tz != "" {
		var err error
		if loc, err = time.LoadLocation(*tz); err != nil {
			return fmt.Errorf("-tz: %w", err)
		}
	}
	// One process, one log: the store hands times back in time.Local, so make that the log's zone.
	time.Local = loc

	files, err := tui.Expand(fs.Args())
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "mailinois-inspect-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "inspect.db")
	st, err := store.Open(ctx, "sqlite:"+dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Anything logged would draw over the screen. Put it back for the error main reports once the screen is gone.
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// An SSH session that drops sends SIGHUP: stop the same way as on q, so the temp directory goes too.
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGHUP)
	defer cancel()
	ld := &tui.Loader{Store: st, Loc: loc, Server: *server, Follow: *followLog}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ld.Run(ctx, files)
	}()
	m := tui.New(ctx, tui.Options{Loader: ld, Files: files, DBPath: dir, QueueMode: *queue, Search: *search})
	_, err = tea.NewProgram(m, tea.WithContext(ctx)).Run()
	cancel()
	<-done
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}
