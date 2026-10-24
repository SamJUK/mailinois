package main

import (
	"compress/gzip"
	"os"
	"path/filepath"
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
