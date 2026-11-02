package tui

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/store/storetest"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if strings.HasSuffix(path, ".gz") {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		zw := gzip.NewWriter(f)
		zw.Write([]byte(text))
		zw.Close()
		f.Close()
		return
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func names(files []string) []string {
	var out []string
	for _, f := range files {
		out = append(out, filepath.Base(f))
	}
	return out
}

func TestExpandDirectoryRotationSet(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"mail.log", "mail.log.1", "mail.log.2.gz", "mail.log.10.gz", "mail.err", "mail.err.1", "mail.warn", "syslog"} {
		write(t, filepath.Join(dir, n), "")
	}
	got, err := Expand([]string{dir})
	want := []string{"mail.log.10.gz", "mail.log.2.gz", "mail.log.1", "mail.log"}
	if err != nil || !slices.Equal(names(got), want) {
		t.Errorf("Expand(dir) = %v, %v; want %v, oldest first, without mail.err or mail.warn", names(got), err, want)
	}
}

func TestExpandDatedRotation(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"maillog", "maillog-20261025", "maillog-20261018.gz", "messages"} {
		write(t, filepath.Join(dir, n), "")
	}
	got, err := Expand([]string{dir})
	want := []string{"maillog-20261018.gz", "maillog-20261025", "maillog"}
	if err != nil || !slices.Equal(names(got), want) {
		t.Errorf("Expand(dir) = %v, %v; want %v", names(got), err, want)
	}
}

func TestExpandGlobFileAndStdin(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"mail.log", "mail.log.1", "other.txt"} {
		write(t, filepath.Join(dir, n), "")
	}
	got, err := Expand([]string{filepath.Join(dir, "mail.log*")})
	if want := []string{"mail.log.1", "mail.log"}; err != nil || !slices.Equal(names(got), want) {
		t.Errorf("glob: %v, %v; want %v", names(got), err, want)
	}
	// A glob over a whole log directory still ends on the live mail.log, the file that's followed.
	logs := t.TempDir()
	for _, n := range []string{"mail.err", "mail.info", "mail.log", "mail.log.1", "mail.warn"} {
		write(t, filepath.Join(logs, n), "")
	}
	got, err = Expand([]string{filepath.Join(logs, "mail*")})
	if want := []string{"mail.err", "mail.info", "mail.warn", "mail.log.1", "mail.log"}; err != nil || !slices.Equal(names(got), want) {
		t.Errorf("glob over a log directory: %v, %v; want %v", names(got), err, want)
	}
	if _, err := Expand([]string{filepath.Join(dir, "nothing*")}); err == nil || !strings.Contains(err.Error(), "nothing*") {
		t.Errorf("a glob matching nothing: %v; want an error naming it", err)
	}
	// Files on the command line are read as given, a named file is only itself, - is stdin.
	got, err = Expand([]string{filepath.Join(dir, "other.txt"), filepath.Join(dir, "mail.log"), "-"})
	if want := []string{"other.txt", "mail.log", "-"}; err != nil || !slices.Equal(names(got), want) {
		t.Errorf("files: %v, %v; want %v", names(got), err, want)
	}
	if _, err := Expand([]string{t.TempDir()}); err == nil || !strings.Contains(err.Error(), "mail.log or maillog") {
		t.Errorf("a directory without a mail log: %v; want an error saying what it looked for", err)
	}
	if _, err := Expand([]string{filepath.Join(dir, "missing.log")}); err == nil {
		t.Error("a missing file expanded without error")
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"Subject: \x1b]52;c;ZWNobyBoaQ==\a hi": `Subject: \x1b]52;c;ZWNobyBoaQ==\x07 hi`,
		"\x1b[2Jcleared":                       `\x1b[2Jcleared`,
		"tab\there\r\n":                        `tab\x09here\x0d\x0a`,
		"c1 \u009b31m":                         `c1 \x9b31m`,
		"bidi \u202egnp.exe":                   `bidi \u202egnp.exe`,
		"bad \xff utf8":                        "bad \uFFFD utf8",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q; want %q", in, got, want)
		}
		if strings.ContainsAny(clean(in), "\x1b\a\u009b\u202e") {
			t.Errorf("clean(%q) still holds a control", in)
		}
	}
	for _, ok := range []string{"Order #1234 ✓ 注文 confirmed", "250 2.0.0 OK  1792352387 - gsmtp", ""} {
		if got := clean(ok); got != ok {
			t.Errorf("clean(%q) = %q; ordinary text should come out unchanged", ok, got)
		}
	}
}

func TestParseQueue(t *testing.T) {
	in := `{"queue_name":"deferred","queue_id":"3F2A81C0B4","arrival_time":1792352387,"message_size":1234,"sender":"shop@store-a.example","recipients":[{"address":"ann@mail.example","delay_reason":"connect to mail.example[192.0.2.10]:25: Connection timed out"}]}
not json
{"queue_name":"active","queue_id":"","sender":"no id"}
{"queue_name":"hold","queue_id":"4A5B6C7D8E","arrival_time":1792352400,"sender":"","recipients":[{"address":"bob@mail.example"}]}
`
	got := parseQueue(strings.NewReader(in))
	if len(got) != 2 || got[0].ID != "3F2A81C0B4" || got[0].Recipients[0].Reason == "" || got[1].Queue != "hold" {
		t.Errorf("parseQueue = %+v; want the two good lines, skipping the bad ones", got)
	}
}

func TestChooseQueueOnlyForThisMachine(t *testing.T) {
	if !fromHere("relay-1.example.test", []string{"relay-1"}) {
		t.Error("relay-1.example.test should match the log's relay-1")
	}
	if fromHere("laptop.local", []string{"relay-1", "relay-2"}) {
		t.Error("a laptop reading a relay's logs matched as the relay")
	}
	if src, note := ChooseQueue("off", nil); src != nil || note == "" {
		t.Errorf("-queue off: %v, %q", src, note)
	}
}

// demoLog is made-up traffic: one delivered, one bounced, one deferred and still queued, and one whose subject
// and remote answer try to drive the terminal.
const demoLog = `Oct 18 09:00:00 relay-1 postfix/smtpd[100]: 0DFD162666: client=app-1[192.0.2.4]
Oct 18 09:00:00 relay-1 postfix/cleanup[101]: 0DFD162666: message-id=<a1@acme.test>
Oct 18 09:00:00 relay-1 postfix/qmgr[90]: 0DFD162666: from=<hello@acme.test>, size=673, nrcpt=1 (queue active)
Oct 18 09:00:01 relay-1 postfix/smtp[102]: 0DFD162666: to=<ann@mail.example>, relay=mx.mail.example[192.0.2.20]:25, delay=0.78, delays=0.1/0/0.1/0.5, dsn=2.0.0, status=sent (250 2.0.0 OK delivered-marker)
Oct 18 09:00:01 relay-1 postfix/qmgr[90]: 0DFD162666: removed
Oct 18 09:01:00 relay-1 postfix/smtpd[100]: 1A2B3C4D5E: client=app-1[192.0.2.4]
Oct 18 09:01:00 relay-1 postfix/cleanup[101]: 1A2B3C4D5E: message-id=<b2@acme.test>
Oct 18 09:01:00 relay-1 postfix/qmgr[90]: 1A2B3C4D5E: from=<hello@acme.test>, size=700, nrcpt=1 (queue active)
Oct 18 09:01:01 relay-1 postfix/smtp[102]: 1A2B3C4D5E: to=<gone@mail.example>, relay=mx.mail.example[192.0.2.20]:25, delay=0.5, delays=0.1/0/0.1/0.3, dsn=5.1.1, status=bounced (550 5.1.1 user unknown bounce-marker)
Oct 18 09:01:01 relay-1 postfix/qmgr[90]: 1A2B3C4D5E: removed
Oct 18 09:02:00 relay-1 postfix/smtpd[100]: 2B3C4D5E6F: client=app-1[192.0.2.4]
Oct 18 09:02:00 relay-1 postfix/cleanup[101]: 2B3C4D5E6F: message-id=<c3@acme.test>
Oct 18 09:02:00 relay-1 postfix/cleanup[101]: 2B3C4D5E6F: info: header Subject: evil` + "\x1b]52;c;aGk=\a\x1b[2J" + ` from app-1[192.0.2.4]; from=<hello@acme.test> to=<slow@mail.example> proto=ESMTP helo=<app-1>
Oct 18 09:02:00 relay-1 postfix/qmgr[90]: 2B3C4D5E6F: from=<hello@acme.test>, size=800, nrcpt=1 (queue active)
Oct 18 09:02:05 relay-1 postfix/smtp[102]: 2B3C4D5E6F: to=<slow@mail.example>, relay=none, delay=5, delays=0.1/0/5/0, dsn=4.4.1, status=deferred (connect to mx.mail.example[192.0.2.20]:25: Connection timed out deferred-marker)
`

func loaded(t *testing.T, follow bool, files ...string) (*Loader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ld := &Loader{Store: storetest.SQLite(t), Loc: time.UTC, Follow: follow, Poll: 10 * time.Millisecond}
	go ld.Run(ctx, files)
	deadline := time.Now().Add(5 * time.Second)
	for !ld.loaded.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the loader never finished its first read")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(cancel)
	return ld, cancel
}

// screen runs Update until a fetch has landed, then returns what View draws, without styling.
func screen(t *testing.T, m *Model, keys ...string) string {
	t.Helper()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	settle := func() {
		for range 5 {
			if cmd := m.refresh(); cmd != nil {
				m.Update(cmd())
			}
		}
	}
	settle()
	for _, k := range keys {
		_, cmd := m.Update(press(k))
		for cmd != nil {
			msg := cmd()
			if _, ok := msg.(tea.BatchMsg); ok {
				break
			}
			_, cmd = m.Update(msg)
		}
		settle()
	}
	return ansi.Strip(m.render())
}

func press(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	}
	if c, ok := strings.CutPrefix(k, "ctrl+"); ok {
		return tea.KeyPressMsg{Code: []rune(c)[0], Mod: tea.ModCtrl}
	}
	return tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
}

func TestScreens(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "mail.log.1.gz"), demoLog[:strings.Index(demoLog, "Oct 18 09:02:00")])
	write(t, filepath.Join(dir, "mail.log"), demoLog[strings.Index(demoLog, "Oct 18 09:02:00"):])
	files, err := Expand([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	ld, _ := loaded(t, false, files...)
	if p := ld.Progress(); p.Lines != 15 || p.Err != nil {
		t.Fatalf("read %d lines, %v; want all 15 across the gzipped and plain file", p.Lines, p.Err)
	}

	m := New(context.Background(), Options{Loader: ld, Files: files, QueueMode: "off"})
	over := screen(t, m)
	for _, want := range []string{"Received 3", "Delivered 1", "Bounced 1", "Queued 1", "4.4.1"} {
		if !strings.Contains(over, want) {
			t.Errorf("overview lacks %q:\n%s", want, over)
		}
	}
	if label := m.filesLabel(); !strings.HasSuffix(label, "/: mail.log.1.gz … mail.log (2 files)") {
		t.Errorf("files label %q; want the directory and its two files", label)
	}
	for i, l := range strings.Split(over, "\n") {
		if w := ansi.StringWidth(l); w > 120 {
			t.Errorf("line %d is %d columns in a 120-column terminal: %q", i, w, l)
		}
	}

	m = New(context.Background(), Options{Loader: ld, Files: files, QueueMode: "off"})
	list := screen(t, m, "2")
	for _, want := range []string{"0DFD162666", "1A2B3C4D5E", "2B3C4D5E6F"} {
		if !strings.Contains(list, want) {
			t.Errorf("messages lack %s:\n%s", want, list)
		}
	}
	bounced := screen(t, m, "/", "s", "t", "a", "t", "u", "s", ":", "b", "o", "u", "n", "c", "e", "d", "enter")
	if !strings.Contains(bounced, "1A2B3C4D5E") || strings.Contains(bounced, "0DFD162666") {
		t.Errorf("status:bounced should list the bounce and hide the delivery:\n%s", bounced)
	}

	// The message whose subject carries escape sequences: they show as text, and the rest of its story is there.
	m = New(context.Background(), Options{Loader: ld, Files: files, QueueMode: "off"})
	m.search = "qid:2B3C4D5E6F"
	story := screen(t, m, "2", "enter")
	raw := m.render()
	if strings.Contains(raw, "\x1b]52") || strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "\a") {
		t.Errorf("the subject's escape sequences reached the terminal:\n%q", raw)
	}
	for _, want := range []string{`evil\x1b]52;c;aGk=\x07\x1b[2J`, "slow@mail.example", "deferred-marker", "postsuper -d 2B3C4D5E6F"} {
		if !strings.Contains(story, want) {
			t.Errorf("the message view lacks %q:\n%s", want, story)
		}
	}
	m = New(context.Background(), Options{Loader: ld, Files: files, QueueMode: "off"})
	m.search = "qid:0DFD162666"
	if done := screen(t, m, "2", "enter"); strings.Contains(done, "postsuper") || !strings.Contains(done, "delivered-marker") {
		t.Errorf("a delivered message offers queue commands, or lacks its story:\n%s", done)
	}

	m = New(context.Background(), Options{Loader: ld, Files: files, QueueMode: "off"})
	q := screen(t, m, "3")
	if !strings.Contains(q, "Queue as seen in the log") || !strings.Contains(q, "2B3C4D5E6F") || strings.Contains(q, "0DFD162666") {
		t.Errorf("queue tab without postqueue should say it's the log's view and list only the deferred message:\n%s", q)
	}
}

func TestFollowAppendAndRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.log")
	head := demoLog[:strings.Index(demoLog, "Oct 18 09:01:00")]
	write(t, path, head)
	ld, _ := loaded(t, true, path)
	waitLines := func(n int64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for ld.Lines.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("read %d lines; want %d", ld.Lines.Load(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitLines(5)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("Oct 18 09:01:00 relay-1 postfix/smtpd[100]: 1A2B3C4D5E: client=app-1[192.0.2.4]\n")
	f.Close()
	waitLines(6)

	os.Rename(path, path+".1")
	write(t, path, "Oct 18 09:03:00 relay-1 postfix/qmgr[90]: 9F8E7D6C5B: removed\n")
	waitLines(7)
	time.Sleep(20 * ld.Poll)
	if ld.Lines.Load() != 7 {
		t.Errorf("after rotation: %d lines; want 7, each read once", ld.Lines.Load())
	}
}

func TestStdinIsLoadedAtOnce(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	ld, _ := loaded(t, true, "-")
	w.WriteString(demoLog)
	deadline := time.Now().Add(5 * time.Second)
	for ld.Lines.Load() < 15 || ld.Version.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("read %d lines from a pipe that stays open; want 15, written within a poll", ld.Lines.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Close()
}

func TestQuitDuringLoad(t *testing.T) {
	var b strings.Builder
	for range 200_000 {
		b.WriteString("Oct 18 09:00:00 relay-1 postfix/qmgr[90]: 0DFD162666: removed\n")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "mail.log.1"), b.String())
	write(t, filepath.Join(dir, "mail.log"), b.String())
	// Quitting in an older file (read once) and in the newest (followed): each stops part way.
	for _, files := range [][]string{{filepath.Join(dir, "mail.log.1"), filepath.Join(dir, "mail.log")}, {filepath.Join(dir, "mail.log")}} {
		ctx, cancel := context.WithCancel(context.Background())
		ld := &Loader{Store: storetest.SQLite(t), Loc: time.UTC, Follow: true}
		done := make(chan struct{})
		go func() { ld.Run(ctx, files); close(done) }()
		for ld.Lines.Load() < 1000 {
			time.Sleep(time.Millisecond)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: still loading 2s after quitting", filepath.Base(files[0]))
		}
		if n := ld.Lines.Load(); n >= 200_000 || n < 1000 {
			t.Errorf("%s: read %d lines; want it to stop part way", filepath.Base(files[0]), n)
		}
	}
}

func TestQueueLookupsSurviveARefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.log")
	write(t, path, demoLog)
	ld, _ := loaded(t, false, path)
	m := New(context.Background(), Options{Loader: ld, Files: []string{path}, QueueMode: "off"})
	screen(t, m, "3")
	// 0DFD162666 the log saw delivered and removed, then Postfix requeued it; 7777777777 predates the log.
	m.d.queued = []Queued{{ID: "0DFD162666", Queue: "deferred"}, {ID: "7777777777", Queue: "deferred"}}
	screen(t, m)
	if id, known := m.logged("0DFD162666"); id == 0 || !known {
		t.Errorf("a requeued message the log knows: id %d, known %v", id, known)
	}
	if id, known := m.logged("7777777777"); id != 0 || !known {
		t.Errorf("a message from before the log: id %d, known %v; want looked up and not found", id, known)
	}
	m.stale = true
	screen(t, m)
	if id, _ := m.logged("0DFD162666"); id == 0 {
		t.Error("a refresh forgot the requeued message's story")
	}
}

// The keys people bring from less, vim and other TUIs: Space pages rather than pausing, f pauses.
func TestPagerKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.log")
	write(t, path, demoLog)
	ld, _ := loaded(t, false, path)
	m := New(context.Background(), Options{Loader: ld, Files: []string{path}, QueueMode: "off"})
	screen(t, m, "2")
	m.d.rows = make([]store.MessageRow, 200)
	page := m.bodyHeight()
	for _, c := range []struct {
		keys []string
		want int
	}{
		{[]string{"space"}, page},
		{[]string{"pgdown"}, page},
		{[]string{"ctrl+f"}, page},
		{[]string{"ctrl+d"}, page / 2},
		{[]string{"space", "b"}, 0},
		{[]string{"space", "ctrl+b"}, 0},
		{[]string{"j"}, 1},
	} {
		m.cursor[messagesTab] = 0
		for _, k := range c.keys {
			m.Update(press(k))
		}
		if got := m.cursor[messagesTab]; got != c.want {
			t.Errorf("%v: cursor %d, want %d", c.keys, got, c.want)
		}
	}
	if m.paused {
		t.Error("paging paused following")
	}
	m.Update(press("f"))
	if !m.paused {
		t.Error("f didn't pause following")
	}
	m.Update(press("f"))
	if m.paused {
		t.Error("a second f didn't resume following")
	}

	for _, c := range []struct {
		key  string
		want tab
	}{{"right", queueTab}, {"l", tailTab}, {"l", overviewTab}, {"left", tailTab}, {"h", queueTab}} {
		m.Update(press(c.key))
		if m.tab != c.want {
			t.Errorf("%s: tab %d, want %d", c.key, m.tab, c.want)
		}
	}
}

// A busy log changes on every refresh; a queue bigger than one batch must still all be looked up.
func TestBigQueueIsLookedUpWhileTheLogGrows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.log")
	write(t, path, demoLog)
	ld, _ := loaded(t, false, path)
	m := New(context.Background(), Options{Loader: ld, Files: []string{path}, QueueMode: "off"})
	screen(t, m, "3")
	var q []Queued
	for i := range 450 {
		q = append(q, Queued{ID: fmt.Sprintf("AA%08X", i), Queue: "deferred"})
	}
	m.d.queued = q
	for range 5 {
		ld.Version.Add(1)
		if cmd := m.refresh(); cmd != nil {
			m.Update(cmd())
		}
	}
	if _, known := m.logged(q[449].ID); !known || m.lookupsPending() {
		t.Errorf("the last of 450 queue IDs: known %v, still pending %v", known, m.lookupsPending())
	}

	// One the log had no story for is tried again when the queue is re-read, and only then.
	queued, looked := []Queued{{ID: "0DFD162666"}}, map[string]int64{"0DFD162666": 0}
	if found, _ := findQueued(context.Background(), ld.Store, queued, nil, looked, false); len(found) != 0 {
		t.Errorf("without recheck: %v; want nothing looked up again", found)
	}
	if found, err := findQueued(context.Background(), ld.Store, queued, nil, looked, true); err != nil || found["0DFD162666"] == 0 {
		t.Errorf("with recheck: %v, %v; want its story found", found, err)
	}
}

func TestMessageViewScrollAndLateOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.log")
	write(t, path, demoLog)
	ld, _ := loaded(t, false, path)
	m := New(context.Background(), Options{Loader: ld, Files: []string{path}, QueueMode: "off"})
	m.search = "qid:2B3C4D5E6F"
	screen(t, m, "2", "enter")
	if m.open == nil {
		t.Fatal("enter didn't open the message")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	screen(t, m, "G")
	end := m.scroll
	screen(t, m, "k")
	if m.scroll != max(end-1, 0) || end > len(m.messageLines()) {
		t.Errorf("after G scroll is %d, after k %d; want k to move back up one", end, m.scroll)
	}

	// Enter, then a tab change before the message arrives: it mustn't open over the new tab.
	m = New(context.Background(), Options{Loader: ld, Files: []string{path}, QueueMode: "off"})
	screen(t, m, "2")
	_, cmd := m.Update(press("enter"))
	m.Update(press("3"))
	m.Update(cmd())
	if m.open != nil {
		t.Error("a message that arrived after switching tab opened over the Queue tab")
	}
}

func TestSparkStepCoversThePeriod(t *testing.T) {
	for _, span := range []time.Duration{100 * time.Minute, 30 * 24 * time.Hour, time.Minute, 7*time.Hour + 13*time.Minute} {
		for _, cols := range []int{68, 128, 188} {
			if step := sparkStep(span, cols); time.Duration(cols)*step < span || step%time.Minute != 0 {
				t.Errorf("sparkStep(%s, %d) = %s: %d columns cover %s", span, cols, step, cols, time.Duration(cols)*step)
			}
		}
	}
}

func TestHeaderSaysWhetherItFollows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.log")
	write(t, path, demoLog)
	gz := filepath.Join(t.TempDir(), "mail.log.2.gz")
	write(t, gz, demoLog)
	for file, want := range map[string]string{path: "● following", gz: "read to the end"} {
		ld, _ := loaded(t, true, file)
		m := New(context.Background(), Options{Loader: ld, Files: []string{file}, QueueMode: "off"})
		if got := strings.Split(screen(t, m), "\n")[0]; !strings.Contains(got, want) {
			t.Errorf("%s with -follow: top line %q; want %q", filepath.Base(file), got, want)
		}
	}
}

func TestAutoQueueWaitsForAServerName(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	ld, _ := loaded(t, true, "-")
	m := New(context.Background(), Options{Loader: ld, Files: []string{"-"}, QueueMode: "auto"})
	if got := screen(t, m, "3"); m.qchosen || !strings.Contains(got, "no log lines yet") {
		t.Errorf("auto queue chosen before any line (chosen %v):\n%s", m.qchosen, got)
	}
	w.WriteString(demoLog)
	deadline := time.Now().Add(5 * time.Second)
	for len(ld.Hosts()) == 0 || ld.Version.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no lines arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := screen(t, m); !m.qchosen || !strings.Contains(got, "isn't from this machine") {
		t.Errorf("once relay-1's lines arrived, auto should decide it isn't this machine (chosen %v):\n%s", m.qchosen, got)
	}
}
