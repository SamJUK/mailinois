package main

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadLogPlainAndGzip(t *testing.T) {
	text := "Oct 18 09:00:00 relay-1 postfix/qmgr[1]: 0DFD162666: removed\nnot a log line\n\nOct 18 09:00:01 relay-1 postfix/qmgr[1]: 1A2B3C4D5E: removed\n"
	dir := t.TempDir()
	plain := filepath.Join(dir, "mail.log")
	os.WriteFile(plain, []byte(text), 0o600)
	gz := filepath.Join(dir, "mail.log.2.gz")
	f, _ := os.Create(gz)
	zw := gzip.NewWriter(f)
	zw.Write([]byte(text))
	zw.Close()
	f.Close()

	for _, path := range []string{plain, gz} {
		lines, unparsed, err := readLog(path, time.UTC)
		if err != nil || len(lines) != 2 || unparsed != 1 || lines[1].Msg != "1A2B3C4D5E: removed" {
			t.Errorf("%s: %d lines, %d unparsed, %v; want 2 and 1 (blank lines aren't counted)", filepath.Base(path), len(lines), unparsed, err)
		}
	}
	if _, _, err := readLog(filepath.Join(dir, "missing.log"), time.UTC); err == nil {
		t.Error("a missing file read without error")
	}
}

func TestInspectNeedsATerminal(t *testing.T) {
	// go test's stdout isn't a terminal. The positive control, a real terminal, is run by hand in tmux.
	err := runInspect(t.Context(), []string{t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Errorf("inspect with stdout to a pipe: %v; want the needs-a-terminal error", err)
	}
	if err := runInspect(t.Context(), []string{"-queue", "sometimes"}); err == nil || !strings.Contains(err.Error(), "-queue") {
		t.Errorf("-queue sometimes: %v; want it refused", err)
	}
}
