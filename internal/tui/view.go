package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/SamJUK/mailinois/internal/store"
)

const minW, minH = 80, 24

var (
	bold     = lipgloss.NewStyle().Bold(true)
	dim      = lipgloss.NewStyle().Faint(true)
	selected = lipgloss.NewStyle().Reverse(true)
	green    = lipgloss.NewStyle().Foreground(lipgloss.Green)
	yellow   = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	red      = lipgloss.NewStyle().Foreground(lipgloss.Red)
	blue     = lipgloss.NewStyle().Foreground(lipgloss.Blue)
)

func statusStyle(status string) lipgloss.Style {
	switch status {
	case "sent":
		return green
	case "deferred":
		return yellow
	case "bounced", "expired", "rejected":
		return red
	case "queued":
		return blue
	}
	return lipgloss.NewStyle()
}

func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "mailinois inspect"
	return v
}

func (m *Model) render() string {
	if m.w < minW || m.h < minH {
		return fmt.Sprintf("terminal too small (%d×%d); mailinois inspect needs %d×%d", m.w, m.h, minW, minH)
	}
	p := m.ld.Progress()
	if !p.Loaded {
		return m.loading(p)
	}
	var body []string
	switch {
	case m.help:
		body = helpLines()
	case m.notInLog != "":
		body = m.notInLogLines()
	case m.open != nil:
		body = m.messageLines()
		body = body[min(m.scroll, max(len(body)-m.bodyHeight(), 0)):]
	default:
		switch m.tab {
		case overviewTab:
			body = m.overviewLines()
		case messagesTab:
			body = m.messagesLines()
		case queueTab:
			body = m.queueLines()
		case tailTab:
			body = m.tailLines()
		}
	}
	h := m.bodyHeight()
	if len(body) > h {
		body = body[:h]
	}
	for len(body) < h {
		body = append(body, "")
	}
	lines := append(append(m.headerLines(p), body...), m.footer())
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.w, "…")
	}
	return strings.Join(lines, "\n")
}

func (m *Model) bodyHeight() int { return m.h - 3 }

func (m *Model) loading(p Progress) string {
	rate := float64(p.Lines) / max(time.Since(m.started).Seconds(), 0.001)
	lines := []string{
		bold.Render("mailinois inspect"),
		"",
		"Reading " + clean(p.File) + " (" + strconv.Itoa(m.fileIndex(p.File)+1) + " of " + strconv.Itoa(len(m.files)) + ")",
		fmt.Sprintf("%s lines, %s a second", num(int(p.Lines)), num(int(rate))),
		"",
		dim.Render("The database is a throwaway copy in " + clean(m.dbPath) + ", removed when you quit."),
		dim.Render("q quits"),
	}
	return strings.Join(lines, "\n")
}

func (m *Model) fileIndex(f string) int {
	for i, x := range m.files {
		if x == f {
			return i
		}
	}
	return 0
}

func (m *Model) headerLines(p Progress) []string {
	state := green.Render("● following")
	switch {
	case p.Err != nil:
		state = red.Render("stopped: " + clean(p.Err.Error()))
	case m.paused:
		state = yellow.Render("paused")
	case !m.ld.Following.Load():
		state = dim.Render("read to the end")
	}
	period := ""
	if !p.First.IsZero() {
		period = p.First.Format("2 Jan 15:04") + " to " + p.Last.Format("2 Jan 15:04")
	}
	top := fmt.Sprintf("%s  %s  %s lines  %s  %s", bold.Render("mailinois inspect"), state, num(int(p.Lines)), period, m.filesLabel())
	var tabs []string
	for i, name := range tabNames {
		label := fmt.Sprintf(" %d %s ", i+1, name)
		if tab(i) == m.tab && m.open == nil {
			label = selected.Render(label)
		}
		tabs = append(tabs, label)
	}
	second := strings.Join(tabs, " ")
	if m.search != "" {
		second += "   search: " + bold.Render(clean(m.search))
	}
	if m.tab == messagesTab && m.status > 0 {
		second += "   status: " + bold.Render(statuses[m.status].label)
	}
	return []string{top, second}
}

func (m *Model) stdin() bool { return len(m.files) > 0 && m.files[len(m.files)-1] == "-" }

// filesLabel names what was read, so a wrong guess about the rotation set is plain to see.
func (m *Model) filesLabel() string {
	if len(m.files) == 1 {
		if m.files[0] == "-" {
			return "stdin"
		}
		return clean(home(m.files[0]))
	}
	dir := home(filepath.Dir(m.files[0]))
	same := true
	for _, f := range m.files {
		same = same && home(filepath.Dir(f)) == dir
	}
	first, last := filepath.Base(m.files[0]), filepath.Base(m.files[len(m.files)-1])
	if same {
		return clean(fmt.Sprintf("%s/: %s … %s (%d files)", dir, first, last, len(m.files)))
	}
	return clean(fmt.Sprintf("%s … %s (%d files)", home(m.files[0]), home(m.files[len(m.files)-1]), len(m.files)))
}

func home(path string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if rest, ok := strings.CutPrefix(path, h); ok && (rest == "" || rest[0] == filepath.Separator) {
			return "~" + rest
		}
	}
	return path
}

func (m *Model) footer() string {
	if m.editing {
		return "/ " + clean(m.input) + "█   " + dim.Render("enter search · esc cancel · ctrl+u clear")
	}
	keys := "1-4 tabs · / search · enter open · esc back · f follow · ? help · q quit"
	if m.tab == messagesTab {
		keys = "s status · " + keys
	}
	if m.err != nil {
		return red.Render(clean(m.err.Error())) + "   " + dim.Render(keys)
	}
	return dim.Render(keys)
}

func helpLines() []string {
	return []string{
		bold.Render("Keys"),
		"  1 2 3 4, tab     Overview, Messages, Queue, Tail;  ← → h l  previous, next",
		"  /                search, in the dashboard's language: to:a@example.com status:bounced after:6h",
		"  esc              back, or clear the search",
		"  enter            open the message (or, on Overview, the messages behind a deferral reason)",
		"  j k ↑ ↓ wheel    move;  g G home end  top, bottom",
		"  space b          a page down, up (pgdn pgup, ctrl+f ctrl+b too)",
		"  ctrl+d ctrl+u    half a page",
		"  s                Messages: all, failed, deferred, queued, sent",
		"  f                pause or resume following",
		"  q, ctrl+c        quit (the throwaway database goes with it)",
		"",
		bold.Render("Read-only"),
		"  Nothing here changes the queue. A queued message shows the postsuper and postqueue lines to run yourself.",
	}
}

// window is the slice of n rows to show in height lines with the cursor on screen.
func window(n, cursor, height int) (int, int) {
	start := max(cursor-height+1, 0)
	return start, min(start+height, n)
}

// cell cleans s and fits it to exactly w columns.
func cell(s string, w int) string {
	s = clean(s)
	if ansi.StringWidth(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", max(w-ansi.StringWidth(s), 0))
}

func row(sel bool, style lipgloss.Style, cells ...string) string {
	s := " " + strings.Join(cells, " ")
	if sel {
		return selected.Render(s)
	}
	return style.Render(s)
}

func num(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// duration is d without the zero units Go's String keeps: 5h31m, not 5h31m0s.
func duration(d time.Duration) string {
	s := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func stamp(t time.Time) string { return t.Format("02 Jan 15:04:05") }

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func seconds(f *float64) string {
	if f == nil {
		return "-"
	}
	if *f < 10 {
		return fmt.Sprintf("%.1fs", *f)
	}
	return (time.Duration(*f) * time.Second).String()
}

func (m *Model) overviewLines() []string {
	d := m.d.dash
	count := func(label string, n int, style lipgloss.Style) string {
		return label + " " + style.Render(num(n))
	}
	lines := []string{
		" " + strings.Join([]string{count("Received", d.Received, bold), count("Delivered", d.Sent, green),
			count("Deferrals", d.Deferrals, yellow), count("Bounced", d.Bounced, red), count("Expired", d.Expired, red),
			count("Rejected", d.Rejected, red), count("Queued", d.Queue, blue)}, "  "),
	}
	second := " Delivery time p50 " + seconds(d.P50) + "   p95 " + seconds(d.P95)
	if d.Oldest != nil {
		second += "   oldest queued " + stamp(*d.Oldest) + " (" + ago(*d.Oldest) + " ago)"
	}
	lines = append(lines, second, "")
	sent, failed := make([]int, len(d.Buckets)), make([]int, len(d.Buckets))
	for i, b := range d.Buckets {
		sent[i], failed[i] = b.Sent, b.Deferred+b.Bounced+b.Rejected
	}
	lines = append(lines,
		" Delivered "+green.Render(spark(sent, m.w-12)),
		" Failed    "+red.Render(spark(failed, m.w-12)),
		dim.Render("           one column a "+duration(m.d.step)+", each line on its own scale; failed counts deferrals too"), "")

	reasons := min(len(m.d.reasons), 6)
	lines = append(lines, bold.Render(" Why mail waited")+dim.Render("  (deferrals by provider and DSN; enter lists them)"))
	if len(m.d.reasons) == 0 {
		lines = append(lines, dim.Render("   none"))
	}
	start, end := window(len(m.d.reasons), m.cursor[overviewTab], reasons)
	for i := start; i < end; i++ {
		r := m.d.reasons[i]
		lines = append(lines, row(i == m.cursor[overviewTab], lipgloss.NewStyle(),
			cell(num(r.Count), 7), cell(r.Provider, 12), cell(r.DSN, 7), cell(stamp(r.Last), 15), cell(r.Example, m.w-48)))
	}
	lines = append(lines, "", bold.Render(" Providers")+dim.Render("  attempts, delivered, deferred, failed"))
	for _, p := range m.d.providers {
		rate := ""
		if p.Attempts > 0 {
			rate = fmt.Sprintf("%.1f%% failed", 100*float64(p.Failed)/float64(p.Attempts))
		}
		style := lipgloss.NewStyle()
		if p.Failed > 0 {
			style = red
		}
		lines = append(lines, row(false, lipgloss.NewStyle(), cell(p.Provider, 14), cell(num(p.Attempts), 9),
			green.Render(cell(num(p.Sent), 9)), yellow.Render(cell(num(p.Deferred), 9)), style.Render(cell(num(p.Failed), 9)), dim.Render(rate)))
	}
	return lines
}

func spark(vals []int, width int) string {
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	top := 0
	for _, v := range vals {
		top = max(top, v)
	}
	bars := []rune(" ▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range vals {
		i := 0
		if top > 0 && v > 0 {
			i = 1 + v*(len(bars)-2)/top
		}
		b.WriteRune(bars[i])
	}
	return b.String()
}

func (m *Model) messagesLines() []string {
	rest := m.w - 1 - (15 + 1 + 9 + 1 + 16 + 1 + 15 + 1)
	from, to := rest*2/5, rest-rest*2/5-1
	lines := []string{dim.Render(row(false, lipgloss.NewStyle(), cell("Received", 15), cell("Status", 9), cell("From", from),
		cell("To", to), cell("Domain", 16), cell("Queue ID", 15)))}
	if len(m.d.rows) == 0 {
		return append(lines, dim.Render("  no messages match"))
	}
	start, end := window(len(m.d.rows), m.cursor[messagesTab], m.bodyHeight()-2)
	for i := start; i < end; i++ {
		r := m.d.rows[i]
		recipient := r.Recipient
		if r.Recipients > 1 {
			recipient += fmt.Sprintf(" +%d", r.Recipients-1)
		}
		sender := r.Sender
		if !r.SenderKnown {
			sender = "?"
		} else if sender == "" {
			sender = "<>"
		}
		lines = append(lines, row(i == m.cursor[messagesTab], statusStyle(r.Status), cell(stamp(r.ReceivedAt), 15),
			cell(r.Status, 9), cell(sender, from), cell(recipient, to), cell(providerOf(recipient), 16), cell(r.QueueID, 15)))
	}
	more := fmt.Sprintf("  %d shown", len(m.d.rows))
	if len(m.d.rows) == messageLimit {
		more = fmt.Sprintf("  the newest %d; narrow it with / to see older ones", messageLimit)
	}
	return append(lines, dim.Render(more))
}

func providerOf(address string) string {
	_, domain, _ := strings.Cut(address, "@")
	domain, _, _ = strings.Cut(domain, " ")
	return domain
}

func (m *Model) queueLines() []string {
	live := m.qsrc != nil && m.d.queueErr == nil && !m.d.queueAt.IsZero()
	var lines []string
	switch {
	case live:
		lines = append(lines, bold.Render(" Postfix's queue")+dim.Render(fmt.Sprintf("  postqueue -j at %s, every %s; the search doesn't apply here",
			m.d.queueAt.Format("15:04:05"), queueEvery)))
	case m.qsrc != nil && m.d.queueErr != nil:
		lines = append(lines, bold.Render(" Queue as seen in the log")+"  "+red.Render(clean(m.d.queueErr.Error())))
	case m.qsrc != nil:
		lines = append(lines, dim.Render(" asking postqueue…"))
	default:
		note := m.qnote
		if !m.qchosen {
			note = "no log lines yet to tell whose queue it is"
		}
		lines = append(lines, bold.Render(" Queue as seen in the log")+dim.Render("  not Postfix's own: "+clean(note)))
	}
	if live {
		lines = append(lines, dim.Render(row(false, lipgloss.NewStyle(), cell("Age", 5), cell("Queue", 8), cell("Queue ID", 12),
			cell("From", 28), cell("Recipients and why they wait", m.w-58))))
		if len(m.d.queued) == 0 {
			return append(lines, green.Render("  empty"))
		}
		start, end := window(len(m.d.queued), m.cursor[queueTab], m.bodyHeight()-3)
		for i := start; i < end; i++ {
			q := m.d.queued[i]
			why := fmt.Sprintf("%d", len(q.Recipients))
			if len(q.Recipients) > 0 {
				why = q.Recipients[0].Address
				if q.Recipients[0].Reason != "" {
					why += ": " + q.Recipients[0].Reason
				}
				if len(q.Recipients) > 1 {
					why = fmt.Sprintf("+%d %s", len(q.Recipients)-1, why)
				}
			}
			style := yellow
			switch id, known := m.logged(q.ID); {
			case !known:
				style, why = dim, "looking up · "+why
			case id == 0:
				style, why = dim, "not in log · "+why
			}
			lines = append(lines, row(i == m.cursor[queueTab], style, cell(ago(q.Arrived()), 5), cell(q.Queue, 8),
				cell(q.ID, 12), cell(q.Sender, 28), cell(why, m.w-58)))
		}
		return append(lines, dim.Render(fmt.Sprintf("  %d in the queue", len(m.d.queued))))
	}
	lines = append(lines, dim.Render(row(false, lipgloss.NewStyle(), cell("Received", 15), cell("Queue ID", 12), cell("From", 28),
		cell("Tries", 5), cell("Last answer", m.w-66))))
	if len(m.d.logQueue) == 0 {
		return append(lines, green.Render("  nothing the log says is still queued"))
	}
	start, end := window(len(m.d.logQueue), m.cursor[queueTab], m.bodyHeight()-3)
	for i := start; i < end; i++ {
		q := m.d.logQueue[i]
		lines = append(lines, row(i == m.cursor[queueTab], yellow, cell(stamp(q.ReceivedAt), 15), cell(q.QueueID, 12),
			cell(q.Sender, 28), cell(strconv.Itoa(q.Attempts), 5), cell(strings.TrimSpace(q.DSN+" "+q.Response), m.w-66)))
	}
	return append(lines, dim.Render(fmt.Sprintf("  %d the log hasn't seen leave", len(m.d.logQueue))))
}

func (m *Model) tailLines() []string {
	lines := []string{dim.Render(row(false, lipgloss.NewStyle(), cell("At", 15), cell("Status", 9), cell("DSN", 7),
		cell("Recipient", m.w-52), cell("Server", 16)))}
	if len(m.d.tail) == 0 {
		return append(lines, dim.Render("  no deliveries yet"))
	}
	start, end := window(len(m.d.tail), m.cursor[tailTab], m.bodyHeight()-1)
	for i := start; i < end; i++ {
		t := m.d.tail[i]
		lines = append(lines, row(i == m.cursor[tailTab], statusStyle(t.Status), cell(stamp(t.At), 15), cell(t.Status, 9),
			cell(t.DSN, 7), cell(t.Recipient, m.w-52), cell(t.Server, 16)))
	}
	return lines
}

func (m *Model) notInLogLines() []string {
	q, _ := m.liveQueued(m.notInLog)
	lines := []string{
		bold.Render(" "+clean(q.ID)) + "  " + dim.Render("in Postfix's queue, not in the log: it arrived before the log starts"),
		"",
		" Queue    " + clean(q.Queue),
		" Arrived  " + stamp(q.Arrived()) + " (" + ago(q.Arrived()) + " ago)",
		" From     " + clean(q.Sender),
		" Size     " + num(int(q.Size)) + " bytes",
		"",
	}
	for _, r := range q.Recipients {
		lines = append(lines, " "+clean(r.Address), "   "+yellow.Render(clean(r.Reason)))
	}
	return append(append(lines, ""), commandLines(q.ID)...)
}

func (m *Model) messageLines() []string {
	msg := m.open
	sender := msg.Sender
	if !msg.SenderKnown {
		sender = "? (the log doesn't say)"
	} else if sender == "" {
		sender = "<> (a bounce or notice)"
	}
	lines := []string{
		" " + bold.Render(clean(msg.QueueID)) + "  " + statusStyle(msg.Status).Render(msg.Status) + "  on " + clean(msg.Server),
		"",
		" From        " + clean(sender),
		" Message-ID  " + clean(msg.MessageID),
		" Received    " + stamp(msg.ReceivedAt),
	}
	if msg.Subject != "" {
		lines = slices.Insert(lines, 3, " Subject     "+clean(msg.Subject))
	}
	if msg.RemovedAt != nil {
		lines = append(lines, " Removed     "+stamp(*msg.RemovedAt))
	}
	if client := strings.TrimSpace(msg.ClientHost + " [" + msg.ClientIP + "]"); msg.ClientIP == "" && msg.ClientHost != "" {
		lines = append(lines, " Client      "+clean(msg.ClientHost))
	} else if msg.ClientIP != "" {
		lines = append(lines, " Client      "+clean(client))
	}
	if msg.SASLUser != "" {
		lines = append(lines, " SASL user   "+clean(msg.SASLUser))
	}
	if msg.Size > 0 {
		lines = append(lines, " Size        "+num(int(msg.Size))+" bytes")
	}
	lines = append(lines, "", bold.Render(" Recipients"))
	for _, r := range msg.Recipients {
		detail := strings.TrimSpace(strings.Join([]string{r.DSN, r.Relay}, "  "))
		if r.Delay != nil {
			detail += "  after " + seconds(r.Delay)
		}
		if r.Attempts > 1 {
			detail += fmt.Sprintf("  %d tries", r.Attempts)
		}
		lines = append(lines, " "+statusStyle(r.Status).Render(cell(r.Status, 9))+" "+clean(r.Address)+"  "+dim.Render(clean(detail)))
		if r.Response != "" {
			lines = append(lines, wrap(r.Response, m.w-12, "           ")...)
		}
	}
	lines = append(lines, "", bold.Render(" Log"))
	for _, e := range msg.Events {
		lines = append(lines, wrap(e.At.Format("15:04:05")+" "+e.Program+" "+e.Raw, m.w-4, " ")...)
	}
	if m.stillQueued(msg) {
		lines = append(append(lines, ""), commandLines(msg.QueueID)...)
	}
	return lines
}

// stillQueued is Postfix's word when we have it, and the log's otherwise.
func (m *Model) stillQueued(msg *store.Message) bool {
	if m.qsrc != nil && m.d.queueErr == nil && !m.d.queueAt.IsZero() {
		_, ok := m.liveQueued(msg.QueueID)
		return ok
	}
	return msg.RemovedAt == nil && (msg.Status == "queued" || msg.Status == "deferred")
}

func commandLines(qid string) []string {
	id := clean(qid)
	return []string{
		bold.Render(" To act on it") + dim.Render("  (inspect only reads; these need root)"),
		"   postcat -q " + id + "      # the queued file",
		"   postqueue -i " + id + "    # try it now",
		"   postsuper -h " + id + "    # hold it",
		"   postsuper -d " + id + "    # delete it",
	}
}

// wrap cleans s and breaks it into lines of width, each starting with indent.
func wrap(s string, width int, indent string) []string {
	var out []string
	for _, l := range strings.Split(ansi.Wrap(clean(s), max(width, 20), ""), "\n") {
		out = append(out, indent+dim.Render(l))
	}
	return out
}
