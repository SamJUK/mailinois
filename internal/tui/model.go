// Package tui is mailinois inspect: an interactive terminal view of one node's mail logs, read into a
// throwaway database, with Postfix's live queue beside them.
package tui

import (
	"context"
	"maps"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/SamJUK/mailinois/internal/store"
)

type tab int

const (
	overviewTab tab = iota
	messagesTab
	queueTab
	tailTab
)

var tabNames = []string{"Overview", "Messages", "Queue", "Tail"}

// statuses is what s cycles through on the Messages tab.
var statuses = []struct{ label, value string }{
	{"all", ""},
	{"failed", "bounced,expired,rejected"},
	{"deferred", "deferred"},
	{"queued", "queued,deferred"},
	{"sent", "sent"},
}

const (
	messageLimit = 500
	queueEvery   = 5 * time.Second
	lookupBatch  = 200
	forever      = 100 * 365 * 24 * time.Hour // the log is all there is: no removal line means still queued
)

// Model is the whole screen. Store reads run in commands, so a slow query never freezes the keys.
type Model struct {
	ctx       context.Context
	ld        *Loader
	files     []string
	dbPath    string
	queueMode string
	started   time.Time

	qsrc    *QueueSource
	qnote   string
	qchosen bool

	w, h      int
	tab       tab
	open      *store.Message // the Message view, over whichever tab opened it
	notInLog  string         // queue ID opened from the Queue tab that the log has no story for
	scroll    int            // Message view
	cursor    [4]int
	search    string
	editing   bool
	input     string
	status    int
	paused    bool
	help      bool
	looked    map[string]int64 // every live queue ID looked up so far, as data.found
	fetching  bool
	openSeq   int  // goes up whenever the view changes, so a message that arrives late doesn't open over it
	stale     bool // the search or tab changed since the last fetch
	seen      int64
	lastQueue time.Time
	d         data
	err       error
}

type data struct {
	dash      store.Dashboard
	step      time.Duration
	reasons   []store.Reason
	providers []store.ProviderStats
	rows      []store.MessageRow
	queued    []Queued
	queueErr  error
	queueAt   time.Time
	logQueue  []store.QueuedMessage
	inLog     map[string]int64 // queue ID: message, for what the log says is still queued
	found     map[string]int64 // live queue IDs looked up this fetch: their message, or 0 when the log hasn't one
	tail      []store.TailLine
}

type Options struct {
	Loader    *Loader
	Files     []string
	DBPath    string
	QueueMode string // auto, on or off
	Search    string
}

func New(ctx context.Context, o Options) *Model {
	return &Model{ctx: ctx, ld: o.Loader, files: o.Files, dbPath: o.DBPath, queueMode: o.QueueMode, search: o.Search,
		started: time.Now(), stale: true, w: 80, h: 24}
}

type tickMsg struct{}

type dataMsg struct {
	tab     tab
	d       data
	version int64
	err     error
}

type openMsg struct {
	m   *store.Message
	qid string
	seq int
	err error
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *Model) Init() tea.Cmd { return tick() }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.stale = true
	case tickMsg:
		return m, tea.Batch(tick(), m.refresh())
	case dataMsg:
		m.fetching = false
		m.err = msg.err
		if msg.err == nil {
			m.merge(msg)
		}
		m.seen = msg.version
	case openMsg:
		if msg.seq != m.openSeq {
			return m, nil // the user moved on before it arrived
		}
		m.err = msg.err
		if msg.err == nil {
			m.open, m.notInLog, m.scroll = msg.m, msg.qid, 0
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.move(-3)
		case tea.MouseWheelDown:
			m.move(3)
		}
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

// refresh reads the store again when something was written, the view changed, or the live queue is due.
func (m *Model) refresh() tea.Cmd {
	if m.fetching || m.open != nil || m.notInLog != "" || !m.ld.loaded.Load() {
		return nil
	}
	v := m.ld.Version.Load()
	queueDue := m.tab == queueTab && time.Since(m.lastQueue) >= queueEvery
	if !m.stale && (m.paused || v == m.seen) && !queueDue && !m.lookupsPending() {
		return nil
	}
	m.fetching, m.stale = true, false
	// Auto mode goes by the log's server names, so wait for the first one (stdin may not have sent any yet).
	if hosts := m.ld.Hosts(); m.tab == queueTab && !m.qchosen && (len(hosts) > 0 || m.queueMode != "auto") {
		m.qsrc, m.qnote = ChooseQueue(m.queueMode, hosts)
		m.qchosen = true
	}
	askQueue := m.tab == queueTab && m.qsrc != nil && queueDue
	if askQueue {
		m.lastQueue = time.Now()
	}
	t, f, width, qsrc, height, looked, queued := m.tab, m.filter(), m.w, m.qsrc, m.h, maps.Clone(m.looked), m.d.queued
	return func() tea.Msg {
		d, err := fetch(m.ctx, m.ld.Store, t, f, width, height, qsrc, askQueue, looked, queued)
		return dataMsg{tab: t, d: d, version: v, err: err}
	}
}

func (m *Model) merge(msg dataMsg) {
	switch msg.tab {
	case overviewTab:
		m.d.dash, m.d.step, m.d.reasons, m.d.providers = msg.d.dash, msg.d.step, msg.d.reasons, msg.d.providers
	case messagesTab:
		m.d.rows = msg.d.rows
	case queueTab:
		m.d.logQueue, m.d.inLog = msg.d.logQueue, msg.d.inLog
		if !msg.d.queueAt.IsZero() {
			m.d.queued, m.d.queueErr, m.d.queueAt = msg.d.queued, msg.d.queueErr, msg.d.queueAt
		}
		if m.looked == nil {
			m.looked = map[string]int64{}
		}
		maps.Copy(m.looked, msg.d.found)
	case tailTab:
		m.d.tail = msg.d.tail
	}
	m.cursor[msg.tab] = min(m.cursor[msg.tab], max(m.rowCount(msg.tab)-1, 0))
}

// filter is the whole loaded period, narrowed by the search and the status buttons.
func (m *Model) filter() store.Filter {
	p := m.ld.Progress()
	f := store.Filter{Since: p.First, Until: p.Last.Add(time.Microsecond)}
	if p.First.IsZero() {
		f.Since, f.Until = time.Now(), time.Now()
	}
	q := store.ParseSearch(m.search, time.Now())
	f.Terms = q.Terms
	if q.After != nil {
		f.Since = *q.After
	}
	if q.Before != nil {
		f.Until = *q.Before
	}
	if m.tab == messagesTab && statuses[m.status].value != "" {
		f.Terms = append(f.Terms, store.Term{Field: "status", Value: statuses[m.status].value})
	}
	return f
}

func fetch(ctx context.Context, st *store.Store, t tab, f store.Filter, width, height int, qsrc *QueueSource, askQueue bool, looked map[string]int64, queued []Queued) (data, error) {
	var d data
	var err error
	switch t {
	case overviewTab:
		d.step = sparkStep(f.Until.Sub(f.Since), width-12)
		if d.dash, err = st.Dashboard(ctx, f, d.step, forever); err != nil {
			return d, err
		}
		if d.reasons, err = st.DeferralReasons(ctx, f); err != nil {
			return d, err
		}
		d.providers, err = st.Providers(ctx, f)
	case messagesTab:
		d.rows, err = st.Messages(ctx, f, 0, messageLimit)
	case queueTab:
		if d.logQueue, err = st.Queue(ctx, f, forever, 5000); err != nil {
			return d, err
		}
		d.inLog = map[string]int64{}
		for _, q := range d.logQueue {
			d.inLog[q.QueueID] = q.ID
		}
		if askQueue {
			d.queued, d.queueErr = qsrc.Read(ctx)
			d.queueAt = time.Now()
		}
		if askQueue {
			queued = d.queued
		}
		if d.found, err = findQueued(ctx, st, queued, d.inLog, looked, askQueue); err != nil {
			return d, err
		}
	case tailTab:
		d.tail, err = st.Tail(ctx, f, 0, max(height, 50))
	}
	return d, err
}

// findQueued looks up live queue rows the log's own queue doesn't list, such as a message the log saw removed
// but Postfix still holds (a requeue). Up to lookupBatch a fetch, so a big queue fills in over a few seconds
// rather than freezing the tab; the rest show as still being looked up. IDs never looked up go first; with
// recheck, those the log had no story for are tried again with what's left, as the log may have one now.
func findQueued(ctx context.Context, st *store.Store, queued []Queued, inLog, looked map[string]int64, recheck bool) (map[string]int64, error) {
	found := map[string]int64{}
	for _, again := range []bool{false, true} {
		if again && !recheck {
			break
		}
		for _, q := range queued {
			if len(found) >= lookupBatch {
				return found, nil
			}
			if _, ok := inLog[q.ID]; ok {
				continue
			}
			if id, ok := looked[q.ID]; ok != again || id != 0 {
				continue
			}
			rows, err := st.Messages(ctx, store.Filter{Until: time.Now().Add(forever), Terms: []store.Term{{Field: "qid", Value: q.ID}}}, 0, 1)
			if err != nil {
				return found, err
			}
			found[q.ID] = 0
			if len(rows) > 0 {
				found[q.ID] = rows[0].ID
			}
		}
	}
	return found, nil
}

func (m *Model) lookupsPending() bool {
	if m.tab != queueTab {
		return false
	}
	for _, q := range m.d.queued {
		if _, known := m.logged(q.ID); !known {
			return true
		}
	}
	return false
}

// logged says what the log knows of a live queue ID: its message, and whether it has been looked up yet.
func (m *Model) logged(qid string) (id int64, known bool) {
	if id, ok := m.d.inLog[qid]; ok {
		return id, true
	}
	id, known = m.looked[qid]
	return id, known
}

func sparkStep(span time.Duration, cols int) time.Duration {
	cols = max(cols, 10)
	// Rounded up, so the whole period fits in cols.
	step := (span + time.Duration(cols) - 1) / time.Duration(cols)
	return max((step + time.Minute - 1).Truncate(time.Minute), time.Minute)
}

func (m *Model) openMessage(id int64) tea.Cmd {
	st, seq := m.ld.Store, m.openSeq
	return func() tea.Msg {
		msg, err := st.Message(m.ctx, store.Filter{}, id)
		return openMsg{m: msg, seq: seq, err: err}
	}
}

func (m *Model) rowCount(t tab) int {
	switch t {
	case overviewTab:
		return len(m.d.reasons)
	case messagesTab:
		return len(m.d.rows)
	case queueTab:
		if m.qsrc != nil && m.d.queueErr == nil && !m.d.queueAt.IsZero() {
			return len(m.d.queued)
		}
		return len(m.d.logQueue)
	case tailTab:
		return len(m.d.tail)
	}
	return 0
}

func (m *Model) move(n int) {
	if m.open != nil {
		m.scroll = max(min(m.scroll+n, len(m.messageLines())-m.bodyHeight()), 0)
		return
	}
	m.cursor[m.tab] = max(min(m.cursor[m.tab]+n, m.rowCount(m.tab)-1), 0)
}

func (m *Model) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if m.editing {
		switch s {
		case "enter":
			m.search, m.editing, m.stale = strings.TrimSpace(m.input), false, true
			m.cursor = [4]int{}
			m.openSeq++
			if m.tab == overviewTab || m.tab == queueTab {
				m.tab = messagesTab
			}
		case "esc":
			m.editing = false
		case "backspace":
			if r := []rune(m.input); len(r) > 0 {
				m.input = string(r[:len(r)-1])
			}
		case "ctrl+u":
			m.input = ""
		case "ctrl+c":
			return m, tea.Quit
		default:
			if k.Text != "" {
				m.input += k.Text
			}
		}
		return m, nil
	}
	body := m.bodyHeight()
	switch s {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.help = !m.help
	case "1", "2", "3", "4":
		m.setTab(tab(s[0] - '1'))
	case "tab", "right", "l":
		m.setTab((m.tab + 1) % 4)
	case "shift+tab", "left", "h":
		m.setTab((m.tab + 3) % 4)
	case "/":
		m.editing, m.input = true, m.search
	case "esc":
		switch {
		case m.help:
			m.help = false
		case m.open != nil || m.notInLog != "":
			m.open, m.notInLog = nil, ""
			m.stale = true
			m.openSeq++
		case m.search != "":
			m.search, m.stale = "", true
		}
	case "s":
		if m.tab == messagesTab && m.open == nil {
			m.status = (m.status + 1) % len(statuses)
			m.stale, m.cursor[messagesTab] = true, 0
		}
	case "f":
		m.paused = !m.paused
		m.stale = !m.paused
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "ctrl+d":
		m.move(body / 2)
	case "ctrl+u":
		m.move(-body / 2)
	case "pgdown", "space", "ctrl+f":
		m.move(body)
	case "pgup", "b", "ctrl+b":
		m.move(-body)
	case "g", "home":
		m.move(-1 << 30)
	case "G", "end":
		m.move(1 << 30)
	case "enter":
		return m, m.enter()
	}
	return m, nil
}

func (m *Model) setTab(t tab) {
	m.tab, m.open, m.notInLog, m.stale, m.help = t, nil, "", true, false
	m.openSeq++
}

func (m *Model) enter() tea.Cmd {
	if m.open != nil || m.notInLog != "" {
		return nil
	}
	i := m.cursor[m.tab]
	switch m.tab {
	case overviewTab:
		if i < len(m.d.reasons) {
			r := m.d.reasons[i]
			m.search = "provider:" + r.Provider
			if r.DSN != "" {
				m.search += " dsn:" + r.DSN
			}
			m.status, m.cursor[messagesTab] = 0, 0
			m.setTab(messagesTab)
		}
	case messagesTab:
		if i < len(m.d.rows) {
			return m.openMessage(m.d.rows[i].ID)
		}
	case queueTab:
		if m.qsrc != nil && m.d.queueErr == nil && !m.d.queueAt.IsZero() {
			if i < len(m.d.queued) {
				q := m.d.queued[i]
				switch id, known := m.logged(q.ID); {
				case id != 0:
					return m.openMessage(id)
				case known:
					m.notInLog = q.ID
				}
			}
		} else if i < len(m.d.logQueue) {
			return m.openMessage(m.d.logQueue[i].ID)
		}
	case tailTab:
		if i < len(m.d.tail) {
			return m.openMessage(m.d.tail[i].MessageID)
		}
	}
	return nil
}

// liveQueued reports whether Postfix's queue holds id right now, as of the last postqueue -j.
func (m *Model) liveQueued(id string) (Queued, bool) {
	for _, q := range m.d.queued {
		if q.ID == id {
			return q, true
		}
	}
	return Queued{}, false
}
