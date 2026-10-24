package syslog

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	now := time.Date(2026, 10, 18, 21, 0, 0, 0, time.UTC)
	london, _ := time.LoadLocation("Europe/London")
	msg := "0DFD162666: removed"
	for _, tc := range []struct {
		name, in string
		loc      *time.Location
		want     Line
	}{
		{"RFC 5424 from rsyslog", "<22>1 2026-10-18T19:39:47.825711+00:00 mail.example.net postfix/qmgr 1042 - - " + msg, time.UTC,
			Line{"mail.example.net", time.Date(2026, 10, 18, 19, 39, 47, 825711000, time.UTC), "postfix/qmgr", 1042, msg}},
		{"RFC 5424 with rsyslog's leading space", "<22>1 2026-10-18T19:39:47Z relay postfix 1042 - -  " + msg, time.UTC,
			Line{"relay", time.Date(2026, 10, 18, 19, 39, 47, 0, time.UTC), "postfix", 1042, msg}},
		{"rsyslog ForwardFormat", "<22>2026-10-18T19:39:47.825711+00:00 relay postfix/qmgr[1042]: " + msg, time.UTC,
			Line{"relay", time.Date(2026, 10, 18, 19, 39, 47, 825711000, time.UTC), "postfix/qmgr", 1042, msg}},
		{"RFC 5424 with structured data", `<22>1 2026-10-18T19:39:47Z relay postfix/qmgr 1042 ID47 [origin ip="10.1.0.6" x="a \"]\" b"] ` + msg, time.UTC,
			Line{"relay", time.Date(2026, 10, 18, 19, 39, 47, 0, time.UTC), "postfix/qmgr", 1042, msg}},
		{"RFC 3164 over the wire", "<22>Oct 18 19:39:47 relay postfix/qmgr[1042]: " + msg, time.UTC,
			Line{"relay", time.Date(2026, 10, 18, 19, 39, 47, 0, time.UTC), "postfix/qmgr", 1042, msg}},
		{"classic mail.log in local time", "Oct  8 19:39:47 relay postfix/qmgr[1042]: " + msg, london,
			Line{"relay", time.Date(2026, 10, 8, 18, 39, 47, 0, time.UTC), "postfix/qmgr", 1042, msg}},
		{"December read in January is last year", "Dec 31 23:59:59 relay postfix/qmgr[1042]: " + msg, time.UTC,
			Line{"relay", time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC), "postfix/qmgr", 1042, msg}},
		{"rsyslog file format", "2026-10-18T19:39:47.825711+00:00 relay postfix/qmgr[1042]: " + msg, time.UTC,
			Line{"relay", time.Date(2026, 10, 18, 19, 39, 47, 825711000, time.UTC), "postfix/qmgr", 1042, msg}},
		{"Postfix's own maillog_file, day zero-padded", "Oct 08 19:39:47 relay postfix/qmgr[1042]: " + msg, london,
			Line{"relay", time.Date(2026, 10, 8, 18, 39, 47, 0, time.UTC), "postfix/qmgr", 1042, msg}},
		{"boky/postfix stdout", "2026-10-18T19:39:47.825711+00:00 INFO    postfix/qmgr[1042]: " + msg, time.UTC,
			Line{"", time.Date(2026, 10, 18, 19, 39, 47, 825711000, time.UTC), "postfix/qmgr", 1042, msg}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := now
			if strings.HasPrefix(tc.in, "Dec") {
				at = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			}
			got, ok := Parse(tc.in+"\n", tc.loc, at)
			if !ok || got.Host != tc.want.Host || !got.Time.Equal(tc.want.Time) || got.Program != tc.want.Program || got.PID != tc.want.PID || got.Msg != tc.want.Msg {
				t.Errorf("Parse = %+v, %v\nwant    %+v", got, ok, tc.want)
			}
		})
	}
	for _, junk := range []string{"", "hello", "2026-10-18 not a line", "<22>1 nonsense"} {
		if l, ok := Parse(junk, time.UTC, now); ok {
			t.Errorf("Parse(%q) = %+v, want false", junk, l)
		}
	}
}

func TestReadFrames(t *testing.T) {
	line := "<22>1 2026-10-18T19:39:47Z relay postfix/qmgr 1 - - A: removed"
	in := strconv.Itoa(len(line)) + " " + line + "\n" + // octet-counted; the newline after it is outside the count
		"<22>Oct 18 19:39:47 relay postfix/qmgr[1]: B: removed\n" +
		"2026-10-18T19:39:47Z relay postfix/qmgr[1]: C: removed\n"
	var got []string
	if err := ReadFrames(strings.NewReader(in), func(s string) { got = append(got, strings.TrimSpace(s)) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != line || !strings.HasSuffix(got[1], "B: removed") || !strings.HasSuffix(got[2], "C: removed") {
		t.Errorf("frames = %q", got)
	}
}
