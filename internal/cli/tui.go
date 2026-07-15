package cli

// cmdChat implements `ppz chat` (alias `ppz tui`) — a live roster (a streaming
// `ppz who`) plus a per-agent / per-pipe chat, all in one alt-screen view.
//
// Data model (see the design notes in the PR):
//   - Roster (left, AGENTS section) is polled from IPCWho every few seconds;
//     status dots come from the same classifier `ppz who` uses.
//   - A collared "DM" with alice is two subjects stitched together: my sends
//     go to alice.inbox (local-echoed here, never read back), and alice's
//     replies land on MY inbox. We hold ONE follow on <me>.inbox and fan its
//     messages out to the right DM window by the envelope's `sender` field —
//     creating a row if the sender is someone new.
//   - Uncollared pipes are the easy case: one shared subject, followed
//     directly; every sender is shown and my own sends echo back via the
//     follow (so we do NOT local-echo pipe sends).
//
// Cursor semantics: the inbox + pipe follows advance THIS session's cursor as
// they stream (per the design decision — a human watching the TUI is genuinely
// caught up). The unread (n) badges are a live, in-session affordance layered
// on top; they don't try to survive a restart. Give the TUI its own
// $PPZ_SESSION if you don't want it sharing a cursor with a CLI `ppz read`.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/pipescloud/ppz/internal/chatstore"
	"github.com/pipescloud/ppz/internal/cliproto"
	"github.com/pipescloud/ppz/internal/daemon"
)

// ---- palette ---------------------------------------------------------------

// Full 24-bit palette — truecolor terminals (Ghostty, herdr's embedded VT)
// render these exactly. Terminals without truecolor are pinned to a lower
// colour profile in cmdTUI (see forceColorProfile), so lipgloss auto-degrades
// these hex values to the nearest supported colour instead of emitting raw
// \x1b[38;2;…m sequences that e.g. Terminal.app mis-parses into grey boxes.
var (
	tcOnline  = lipgloss.Color("#22c55e")
	tcStale   = lipgloss.Color("#f59e0b")
	tcOffline = lipgloss.Color("#6b7280")
	tcAccent  = lipgloss.Color("#22d3ee")
	tcSelBg   = lipgloss.Color("#1f3a5f")
	tcDim     = lipgloss.Color("#64748b")
	tcBorder  = lipgloss.Color("#334155")
	tcBorderF = lipgloss.Color("#22d3ee")
	tcErr     = lipgloss.Color("#f87171")
)

// ---- model -----------------------------------------------------------------

type tKind int

const (
	kAgent  tKind = iota // pty source: live terminal/harness, heartbeats, DM
	kSource              // message source: a bare inbox (human/service), DM
	kPipe                // uncollared pipe: shared room
)

type tFocus int

const (
	fMenu tFocus = iota
	fChat
)

type tMsg struct {
	id     string // set for optimistic outbound echoes, so a failed send can roll it back
	t      string
	sender string
	text   string
	you    bool
}

type tItem struct {
	kind   tKind
	key    string // handle (agent) or raw target (pipe)
	label  string
	status string // agent liveness: online/stale/offline; "" until first beat
	state  string // agent_state: idle/working/blocked
	// unread is the live in-session display counter. The store's read marker
	// (chatstore last_read_at) is authoritative across restarts and reconciles
	// this on hydrate; within a session the two are kept in step at the call
	// sites (routeInbound/routePipe bump, markRead zeroes both).
	unread int
	msgs   []tMsg
}

type tuiModel struct {
	me      string
	session string
	sock    string
	events  chan tea.Msg
	ctx     context.Context

	agents  []tItem // pty sources (from who + inbound); menu section AGENTS
	sources []tItem // message sources (from the source list); menu section INBOXES
	pipes   []tItem // uncollared pipes; menu section PIPES

	// sourceSet classifies which handles are message-kind sources, so an
	// inbound DM is routed to INBOXES vs AGENTS by its sender's kind.
	sourceSet map[string]bool
	// sourcesLoaded flips true on the first source list; until then the INBOXES
	// header shows a loading spinner (they load asynchronously).
	sourcesLoaded bool
	spin          spinner.Model

	followed    map[string]bool               // pipe targets we already hold a follow on
	pipeCancels map[string]context.CancelFunc // stops a pipe's follow when it's removed

	// store is the durable chat store (history, added pipes, read markers).
	// nil until wired: with it set, ingest/hydrate/mark-read persist across
	// restarts. See chat_store_e2e_test.go — that e2e is RED until it's wired.
	store *chatstore.Store

	sel    int // flat index: agents then pipes
	focus  tFocus
	adding bool
	addTi  textinput.Model
	chatTi textinput.Model
	toast  string

	// vp scrolls the chat body. vpKey tracks which conversation's content
	// it currently holds so we know to jump to the bottom on a switch; it
	// otherwise sticks to the bottom only when already there (so a scrolled-
	// up reader isn't yanked down by an incoming message).
	vp      viewport.Model
	vpReady bool
	vpKey   string

	w, h int
}

func newTUIModel(me, session, sock string, events chan tea.Msg, ctx context.Context) tuiModel {
	ti := textinput.New()
	ti.Placeholder = "message…"
	ti.Prompt = "› "
	ti.CharLimit = 2000

	add := textinput.New()
	add.Placeholder = "pipe name (bare leaf, e.g. standup)"
	add.Prompt = "add pipe › "
	add.CharLimit = 128

	sp := spinner.New()
	sp.Spinner = spinner.Spinner{Frames: []string{"◰", "◳", "◲", "◱"}, FPS: time.Second / 8}
	sp.Style = lipgloss.NewStyle().Foreground(tcDim)

	return tuiModel{
		me: me, session: session, sock: sock, events: events, ctx: ctx,
		sourceSet: map[string]bool{}, spin: sp,
		followed: map[string]bool{}, pipeCancels: map[string]context.CancelFunc{},
		chatTi: ti, addTi: add,
		vp: viewport.New(1, 1),
	}
}

func (m tuiModel) count() int { return len(m.agents) + len(m.sources) + len(m.pipes) }

// flat index runs AGENTS ++ INBOXES(sources) ++ PIPES.
func (m tuiModel) flatItem(i int) tItem {
	if i < len(m.agents) {
		return m.agents[i]
	}
	i -= len(m.agents)
	if i < len(m.sources) {
		return m.sources[i]
	}
	return m.pipes[i-len(m.sources)]
}

func (m *tuiModel) flatPtr(i int) *tItem {
	if i < len(m.agents) {
		return &m.agents[i]
	}
	i -= len(m.agents)
	if i < len(m.sources) {
		return &m.sources[i]
	}
	return &m.pipes[i-len(m.sources)]
}

func (m tuiModel) isSelected(flatIdx int) bool { return flatIdx == m.sel }

func (m tuiModel) menuW() int {
	w := 30
	if w > m.w-24 {
		w = m.w / 3
	}
	if w < 16 {
		w = 16
	}
	return w
}

// ---- events from background goroutines -------------------------------------

type whoMsg struct{ entries []cliproto.WhoEntry }
type inboundMsg struct{ m cliproto.ReadMessage } // landed on <me>.inbox
type pipeInMsg struct {
	pipe string
	m    cliproto.ReadMessage
}
type sourcesMsg struct{ sources []cliproto.Source }
type streamErrMsg struct{ scope, err string }

// sendResultMsg carries an async send's outcome back to Update. On success an
// agent DM is persisted (it can't be re-hydrated from the wire); on failure the
// optimistic echo is rolled back so a message that never left doesn't look
// delivered or survive a restart.
type sendResultMsg struct {
	kind   string            // chatstore kind for agent DMs; "" for pipe sends
	name   string            // window key
	echoID string            // id of the optimistic echo tMsg (for rollback)
	msg    chatstore.Message // outbound to persist on success
	err    string
}

func waitForEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m tuiModel) Init() tea.Cmd { return tea.Batch(waitForEvent(m.events), m.spin.Tick) }

// ---- update ----------------------------------------------------------------

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.Width = (m.w - m.menuW()) - 2
		if m.vp.Width < 1 {
			m.vp.Width = 1
		}
		m.vp.Height = m.h - 8 // title+2 seps+input inside the bordered chat box
		if m.vp.Height < 1 {
			m.vp.Height = 1
		}
		m.vpReady = true
		m.refreshViewport()
		return m, nil

	// --- background stream events: handle, then keep pumping the channel ---
	case whoMsg:
		m.applyWho(msg.entries)
		m.refreshViewport()
		return m, waitForEvent(m.events)
	case inboundMsg:
		m.routeInbound(msg.m)
		m.refreshViewport()
		return m, waitForEvent(m.events)
	case pipeInMsg:
		m.routePipe(msg.pipe, msg.m)
		m.refreshViewport()
		return m, waitForEvent(m.events)
	case sourcesMsg:
		m.applySources(msg.sources)
		m.sourcesLoaded = true // stops the INBOXES loading spinner
		m.refreshViewport()
		return m, waitForEvent(m.events)
	case spinner.TickMsg:
		if m.sourcesLoaded {
			return m, nil // done loading — let the spinner rest
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case streamErrMsg:
		m.toast = msg.scope + ": " + msg.err
		return m, waitForEvent(m.events)

	// --- result of a send Cmd (not from the channel) ---
	case sendResultMsg:
		if msg.err != "" {
			m.toast = "send failed: " + msg.err
			m.rollbackOutbound(msg.kind, msg.name, msg.echoID)
			return m, nil
		}
		if (msg.kind == chatstore.KindAgent || msg.kind == chatstore.KindSource) && m.store != nil {
			_, _ = m.store.Ingest(msg.kind, msg.name, msg.name, msg.msg)
			m.persist() // durable now, not just on a clean quit
		}
		return m, nil

	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			if idx := m.itemAtY(msg.Y, msg.X); idx >= 0 {
				m.sel = idx
				m.focus = fMenu
				m.chatTi.Blur()
				m.markRead()
				m.refreshViewport()
			} else if m.isAddY(msg.Y, msg.X) {
				m.startAdd()
				return m, textinput.Blink
			}
			return m, nil
		}
		// non-left events (wheel up/down) scroll the chat body
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		if m.adding {
			return m.updateAdding(msg)
		}
		return m.updateKeys(msg)
	}
	return m, nil
}

func (m tuiModel) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	m.toast = ""

	// Scroll the chat history. PgUp/PgDn (Fn+↑/↓ on a Mac) work in any focus.
	// Ctrl+U/Ctrl+D (half-page) are only intercepted in menu focus so they
	// don't steal the "clear line" edit key while you're typing a reply —
	// there, use PgUp/PgDn or the trackpad/wheel.
	switch msg.String() {
	case "pgup", "pgdown":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case "ctrl+u", "ctrl+d":
		if m.focus == fMenu {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
	}

	if m.focus == fChat {
		switch msg.String() {
		case "esc", "left":
			m.focus = fMenu
			m.chatTi.Blur()
			return m, nil
		case "enter":
			return m.send()
		}
		var cmd tea.Cmd
		m.chatTi, cmd = m.chatTi.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "j", "down":
		if m.sel < m.count()-1 {
			m.sel++
		}
	case "k", "up":
		if m.sel > 0 {
			m.sel--
		}
	case "enter", "right", "l", "tab":
		if m.count() > 0 {
			m.markRead()
			m.focus = fChat
			m.chatTi.Focus()
			m.refreshViewport()
			return m, textinput.Blink
		}
	case "a", "+":
		// `a` is the advertised shortcut; `+` matches the [+ add pipe]
		// button glyph so pressing it (or clicking the row) also works.
		m.startAdd()
		return m, textinput.Blink
	case "-":
		if m.count() > 0 && m.sel >= len(m.agents)+len(m.sources) {
			m.removePipe(m.sel)
		} else {
			m.toast = "only pipes can be removed"
		}
	}
	m.refreshViewport() // selection or pipe list changed → reload the chat body
	return m, nil
}

func (m tuiModel) updateAdding(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.adding = false
		m.addTi.Blur()
		m.addTi.SetValue("")
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.addTi.Value())
		m.adding = false
		m.addTi.Blur()
		m.addTi.SetValue("")
		if name != "" {
			m.addPipe(name)
		}
		m.refreshViewport()
		return m, nil
	}
	var cmd tea.Cmd
	m.addTi, cmd = m.addTi.Update(msg)
	return m, cmd
}

func (m *tuiModel) startAdd() {
	m.adding = true
	m.focus = fMenu
	m.chatTi.Blur()
	m.addTi.Focus()
}

func (m *tuiModel) markRead() {
	if m.count() == 0 {
		return
	}
	it := m.flatPtr(m.sel)
	it.unread = 0
	if m.store != nil {
		_ = m.store.MarkRead(storeKind(it.kind), it.key)
		m.persist()
	}
}

func storeKind(k tKind) string {
	switch k {
	case kPipe:
		return chatstore.KindPipe
	case kSource:
		return chatstore.KindSource
	default:
		return chatstore.KindAgent
	}
}

// applySources populates the INBOXES section from the source list: message-kind
// sources (excluding self) become DM windows, and sourceSet records them so
// inbound DMs from a message source route to INBOXES instead of AGENTS.
func (m *tuiModel) applySources(srcs []cliproto.Source) {
	for _, s := range srcs {
		if s.Kind != string(cliproto.KindMessage) || s.Handle == m.me {
			continue
		}
		m.sourceSet[s.Handle] = true
		m.upsertSource(s.Handle)
	}
}

// upsertSource ensures a message source has an INBOXES window. If a DM from it
// already landed in AGENTS (it arrived before we knew its kind), migrate that
// history into the source window.
func (m *tuiModel) upsertSource(handle string) {
	agentIdx, srcIdx := -1, -1
	for i := range m.agents {
		if m.agents[i].key == handle {
			agentIdx = i
			break
		}
	}
	for i := range m.sources {
		if m.sources[i].key == handle {
			srcIdx = i
			break
		}
	}
	if agentIdx == -1 && srcIdx != -1 {
		return // already a source, nothing stray to fold in
	}

	// Fold any stray AGENTS window (a DM that arrived before classification)
	// into the source window in the STORE too, not just the model — otherwise a
	// restart rebuilds two windows for one counterparty.
	if m.store != nil && agentIdx != -1 {
		_ = m.store.Migrate(chatstore.KindAgent, chatstore.KindSource, handle)
		m.persist()
	}

	// Build the source item. Prefer the store (authoritative, merged, sorted);
	// fall back to the migrated agent item when there's no store.
	it := tItem{kind: kSource, key: handle, label: handle}
	if m.store != nil {
		msgs, _ := m.store.Messages(chatstore.KindSource, handle)
		for _, sm := range msgs {
			it.msgs = append(it.msgs, toTMsg(sm))
		}
		it.unread, _ = m.store.Unread(chatstore.KindSource, handle)
	} else if agentIdx != -1 {
		it.msgs, it.unread = m.agents[agentIdx].msgs, m.agents[agentIdx].unread
	}

	selKey, hadSel := m.selectedKey()
	if agentIdx != -1 {
		m.agents = append(m.agents[:agentIdx], m.agents[agentIdx+1:]...)
	}
	if srcIdx != -1 {
		m.sources[srcIdx] = it
	} else {
		m.sources = append(m.sources, it)
	}
	// Keep the highlight on the same logical row across the reshuffle (the
	// migrated handle follows its move from AGENTS to INBOXES).
	if hadSel {
		m.reselectByKey(selKey)
	} else {
		m.clampSel()
	}
}

// selectedKey returns the key of the currently-selected row (for preserving the
// selection across a section reshuffle).
func (m tuiModel) selectedKey() (string, bool) {
	if m.count() == 0 {
		return "", false
	}
	return m.flatItem(m.sel).key, true
}

func (m *tuiModel) reselectByKey(key string) {
	for i := 0; i < m.count(); i++ {
		if m.flatItem(i).key == key {
			m.sel = i
			return
		}
	}
	m.clampSel()
}

func (m *tuiModel) clampSel() {
	if n := m.count(); m.sel > n-1 {
		m.sel = n - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
}

func toTMsg(sm chatstore.Message) tMsg {
	you := sm.Dir == chatstore.DirOut
	sender := sm.Sender
	if you {
		sender = "you"
	}
	return tMsg{t: hm(sm.CreatedAt), sender: sender, text: sm.Payload, you: you}
}

// hydrate loads persisted windows (history + read state + added pipes) into the
// model on launch, and re-follows any hydrated pipes. Subsequent follow
// replays reconcile against the store (Ingest returns added=false), so nothing
// duplicates.
func (m *tuiModel) hydrate() {
	if m.store == nil {
		return
	}
	wins, err := m.store.Windows()
	if err != nil {
		return
	}
	for _, w := range wins {
		msgs, _ := m.store.Messages(w.Kind, w.Name)
		it := tItem{key: w.Name, label: w.Label, unread: w.Unread}
		for _, sm := range msgs {
			it.msgs = append(it.msgs, toTMsg(sm))
		}
		switch w.Kind {
		case chatstore.KindPipe:
			it.kind = kPipe
			m.pipes = append(m.pipes, it)
			if !m.followed[w.Name] {
				m.followed[w.Name] = true
				pctx, cancel := context.WithCancel(m.ctx)
				m.pipeCancels[w.Name] = cancel
				name := w.Name
				go streamRead(pctx, m.sock, buildFollowReq(name, m.session),
					func(rm cliproto.ReadMessage) tea.Msg { return pipeInMsg{name, rm} }, m.events)
			}
		case chatstore.KindSource:
			it.kind = kSource
			m.sourceSet[w.Name] = true
			m.sources = append(m.sources, it)
		default:
			it.kind = kAgent
			m.agents = append(m.agents, it)
		}
	}
}

// addPipe appends a pipe row (if new), selects it, and starts a follow so its
// messages stream in. Idempotent per target.
func (m *tuiModel) addPipe(name string) {
	for j := range m.pipes {
		if m.pipes[j].key == name {
			m.sel = len(m.agents) + len(m.sources) + j
			return
		}
	}
	m.pipes = append(m.pipes, tItem{kind: kPipe, key: name, label: name})
	m.sel = len(m.agents) + len(m.sources) + len(m.pipes) - 1
	if m.store != nil {
		_ = m.store.AddPipe(name, name)
		m.persist()
	}
	if !m.followed[name] {
		m.followed[name] = true
		pctx, cancel := context.WithCancel(m.ctx)
		m.pipeCancels[name] = cancel
		req := buildFollowReq(name, m.session)
		go streamRead(pctx, m.sock, req,
			func(rm cliproto.ReadMessage) tea.Msg { return pipeInMsg{name, rm} }, m.events)
	}
}

// removePipe drops the pipe at the given flat index from the view, stops its
// follow stream, and keeps the selection in range. No-op for agent rows
// (agents come from who/DMs and can't be removed).
func (m *tuiModel) removePipe(flatIdx int) {
	j := flatIdx - len(m.agents) - len(m.sources)
	if j < 0 || j >= len(m.pipes) {
		return
	}
	name := m.pipes[j].key
	if cancel := m.pipeCancels[name]; cancel != nil {
		cancel()
		delete(m.pipeCancels, name)
	}
	delete(m.followed, name)
	if m.store != nil {
		_ = m.store.RemovePipe(name)
		m.persist()
	}
	m.pipes = append(m.pipes[:j], m.pipes[j+1:]...)

	if m.sel > flatIdx {
		m.sel-- // rows below the removed one shifted up
	}
	if m.sel > m.count()-1 {
		m.sel = m.count() - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
}

// send publishes the chat input to the selected conversation. Agent DMs go to
// <handle>.inbox and are local-echoed immediately (we never read them back);
// pipe sends are NOT echoed (the pipe follow delivers them back to us).
func (m tuiModel) send() (tea.Model, tea.Cmd) {
	if m.count() == 0 {
		return m, nil
	}
	text := strings.TrimSpace(m.chatTi.Value())
	if text == "" {
		return m, nil
	}
	it := m.flatPtr(m.sel)
	m.chatTi.SetValue("")
	sock := m.sock
	req := buildSend(it.key, text, m.session, m.me)

	// Pipe sends aren't echoed or stored here — the follow echoes them back and
	// routePipe records them. A failed pipe send just toasts.
	if it.kind == kPipe {
		return m, func() tea.Msg {
			var reply cliproto.SendReply
			if err := daemon.Call(sock, cliproto.IPCSend, req, &reply); err != nil {
				return sendResultMsg{err: err.Error()}
			}
			return sendResultMsg{}
		}
	}

	// DM (agent OR message source): echo optimistically, but persist only once
	// the send is confirmed and roll the echo back if it fails. The store is the
	// sole record of outbound DMs (<handle>.inbox isn't read back — the reply
	// returns on our own inbox), so a failed send must neither look delivered
	// nor survive a restart.
	kind := storeKind(it.kind)
	now := time.Now().UTC()
	out := chatstore.Message{
		ID: "local-" + now.Format(time.RFC3339Nano), Dir: chatstore.DirOut,
		Sender: m.me, Payload: text, CreatedAt: now.Format("2006-01-02T15:04:05Z"),
	}
	it.msgs = append(it.msgs, tMsg{id: out.ID, t: hm(out.CreatedAt), sender: "you", text: text, you: true})
	m.refreshViewport()
	name := it.key
	return m, func() tea.Msg {
		var reply cliproto.SendReply
		if err := daemon.Call(sock, cliproto.IPCSend, req, &reply); err != nil {
			return sendResultMsg{kind: kind, name: name, echoID: out.ID, err: err.Error()}
		}
		return sendResultMsg{kind: kind, name: name, msg: out}
	}
}

// rollbackOutbound removes an optimistic echo from its DM window after the send
// failed (agent or source DMs — pipe sends aren't echoed).
func (m *tuiModel) rollbackOutbound(kind, name, echoID string) {
	if echoID == "" {
		return
	}
	var slice []tItem
	switch kind {
	case chatstore.KindAgent:
		slice = m.agents
	case chatstore.KindSource:
		slice = m.sources
	default:
		return
	}
	for i := range slice {
		if slice[i].key != name {
			continue
		}
		a := &slice[i]
		for j := range a.msgs {
			if a.msgs[j].id == echoID {
				a.msgs = append(a.msgs[:j], a.msgs[j+1:]...)
				break
			}
		}
		break
	}
	m.refreshViewport()
}

// persist flushes the store after an off-hot-path user action (open, send,
// add/remove pipe) so read markers and outbound DMs survive a hard kill, not
// just a clean quit. Inbound stays buffered — it re-hydrates from JetStream.
func (m *tuiModel) persist() {
	if m.store != nil {
		_ = m.store.Flush()
	}
}

// ---- applying live data ----------------------------------------------------

func (m *tuiModel) applyWho(entries []cliproto.WhoEntry) {
	now := time.Now()
	for _, e := range entries {
		if e.Handle == m.me {
			continue // don't list ourselves in the roster
		}
		var p HeartbeatPayload
		_ = json.Unmarshal([]byte(e.Payload), &p)
		status := daemon.ClassifyHeartbeatStatus(e.ArrivedAt, now, p.IntervalSec)
		m.upsertAgent(e.Handle, status, p.AgentState)
	}
}

// upsertAgent updates an existing agent row's status/state or appends a new
// one. Appending shifts the flat indices of the pipe rows, so nudge the
// selection when it was pointing at a pipe (keeps the highlighted row stable).
// Rows are never pruned: a handle that ages out of the who reply keeps its last
// status (who keeps returning it as offline until its cache entry drops), and a
// DM's history should outlive the agent's presence anyway.
func (m *tuiModel) upsertAgent(handle, status, state string) {
	for i := range m.agents {
		if m.agents[i].key == handle {
			m.agents[i].status = status
			m.agents[i].state = state
			return
		}
	}
	oldLen := len(m.agents)
	m.agents = append(m.agents, tItem{kind: kAgent, key: handle, label: handle, status: status, state: state})
	if m.sel >= oldLen && (len(m.sources)+len(m.pipes)) > 0 {
		m.sel++
	}
}

func (m *tuiModel) routeInbound(rm cliproto.ReadMessage) {
	if strings.HasPrefix(rm.Subject, "ack:") {
		return // system ack:read receipts, not chat
	}
	sender := rm.Sender
	if sender == "" {
		sender = "(unknown)"
	}
	// A DM from a known message source belongs in INBOXES; everyone else
	// (agents, and unclassified senders) defaults to AGENTS.
	kind := kAgent
	if m.sourceSet[sender] {
		kind = kSource
	}
	if m.store != nil {
		added, _ := m.store.Ingest(storeKind(kind), sender, sender, chatstore.Message{
			ID: rm.ID, Dir: chatstore.DirIn, Sender: sender,
			Subject: rm.Subject, Payload: rm.Payload, CreatedAt: rm.CreatedAt,
		})
		if !added {
			return // already stored (e.g. a follow replay) → don't duplicate in the model
		}
	}
	it := m.upsertDM(kind, sender)
	it.msgs = append(it.msgs, tMsg{t: hm(rm.CreatedAt), sender: sender, text: rm.Payload})
	if !(m.dmSelected(kind, sender) && m.focus == fChat) {
		it.unread++
	}
}

// upsertDM returns the DM window for (kind, name) — agent or source — creating
// it (and nudging the selection past the insert) if absent.
func (m *tuiModel) upsertDM(kind tKind, name string) *tItem {
	if kind == kSource {
		for i := range m.sources {
			if m.sources[i].key == name {
				return &m.sources[i]
			}
		}
		insertPos := len(m.agents) + len(m.sources)
		m.sources = append(m.sources, tItem{kind: kSource, key: name, label: name})
		if m.sel >= insertPos && len(m.pipes) > 0 {
			m.sel++
		}
		return &m.sources[len(m.sources)-1]
	}
	for i := range m.agents {
		if m.agents[i].key == name {
			return &m.agents[i]
		}
	}
	oldLen := len(m.agents)
	m.agents = append(m.agents, tItem{kind: kAgent, key: name, label: name})
	if m.sel >= oldLen && (len(m.sources)+len(m.pipes)) > 0 {
		m.sel++
	}
	return &m.agents[len(m.agents)-1]
}

// dmSelected reports whether the (kind, name) window is the one currently open
// and focused (so its incoming messages don't bump unread).
func (m tuiModel) dmSelected(kind tKind, name string) bool {
	if m.count() == 0 {
		return false
	}
	it := m.flatItem(m.sel)
	return it.kind == kind && it.key == name
}

func (m *tuiModel) routePipe(pipe string, rm cliproto.ReadMessage) {
	for j := range m.pipes {
		if m.pipes[j].key != pipe {
			continue
		}
		p := &m.pipes[j]
		sender := rm.Sender
		if sender == "" {
			sender = "(unknown)"
		}
		if m.store != nil {
			dir := chatstore.DirIn
			if sender == m.me {
				dir = chatstore.DirOut
			}
			added, _ := m.store.Ingest(chatstore.KindPipe, pipe, pipe, chatstore.Message{
				ID: rm.ID, Dir: dir, Sender: sender,
				Subject: rm.Subject, Payload: rm.Payload, CreatedAt: rm.CreatedAt,
			})
			if !added {
				return
			}
		}
		p.msgs = append(p.msgs, tMsg{t: hm(rm.CreatedAt), sender: sender, text: rm.Payload, you: sender == m.me})
		if !(m.isSelected(len(m.agents)+j) && m.focus == fChat) {
			p.unread++
		}
		return
	}
}

// ---- mouse hit-testing (mirrors renderMenu's layout) -----------------------

// itemAtY mirrors renderMenu's layout across three sections. Screen rows:
// title(0) box-top(1) "AGENTS"(2) agents(3..) blank "INBOXES" sources blank
// "PIPES" pipes blank "[+ add pipe]". A=agents, S=sources, P=pipes.
func (m tuiModel) itemAtY(y, x int) int {
	if x >= m.menuW() {
		return -1
	}
	a, s := len(m.agents), len(m.sources)
	if y >= 3 && y < 3+a {
		return y - 3
	}
	ss := 5 + a // first source row
	if y >= ss && y < ss+s {
		return a + (y - ss)
	}
	ps := 7 + a + s // first pipe row
	if y >= ps && y < ps+len(m.pipes) {
		return a + s + (y - ps)
	}
	return -1
}

func (m tuiModel) isAddY(y, x int) bool {
	if x >= m.menuW() {
		return false
	}
	return y == 8+len(m.agents)+len(m.sources)+len(m.pipes)
}

// ---- view ------------------------------------------------------------------

func (m tuiModel) View() string {
	if m.w == 0 || m.h == 0 {
		return "loading…"
	}
	menuW := m.menuW()
	contentH := m.h - 2
	menu := m.renderMenu(menuW, contentH)
	chat := m.renderChat(m.w-menuW, contentH)
	main := lipgloss.JoinHorizontal(lipgloss.Top, menu, chat)
	return lipgloss.JoinVertical(lipgloss.Left, m.titleBar(), main, m.helpBar())
}

func (m tuiModel) renderMenu(w, h int) string {
	inner := w - 2
	var lines []string
	lines = append(lines, tSectionHeader("AGENTS"))
	for i, a := range m.agents {
		lines = append(lines, m.renderRow(a, i, inner))
	}
	inboxHeader := tSectionHeader("INBOXES")
	if !m.sourcesLoaded {
		inboxHeader += " " + m.spin.View() // loading square while the source list fetches
	}
	lines = append(lines, "", inboxHeader)
	for j, s := range m.sources {
		lines = append(lines, m.renderRow(s, len(m.agents)+j, inner))
	}
	lines = append(lines, "", tSectionHeader("PIPES"))
	for k, p := range m.pipes {
		lines = append(lines, m.renderRow(p, len(m.agents)+len(m.sources)+k, inner))
	}
	lines = append(lines, "", m.addRow())
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tBorderColor(m.focus == fMenu)).
		Width(inner).Height(h - 2)
	return box.Render(strings.Join(lines, "\n"))
}

func (m tuiModel) renderRow(it tItem, flatIdx, w int) string {
	marker := "  "
	if flatIdx == m.sel {
		marker = "▸ "
	}
	// Selected row: a filled navy bar (restores the original look) rendered as
	// plain text — no per-segment colour, so inner ANSI resets can't punch
	// holes in the background. The ▸ caret is a colour-independent backstop so
	// the selection is legible even where the bar degrades to a subtle shade.
	if flatIdx == m.sel {
		count := ""
		if it.unread > 0 {
			count = fmt.Sprintf("(%d) ", it.unread)
		}
		row := tPadRow(marker+statusGlyph(it)+" "+it.label, count, w)
		return lipgloss.NewStyle().Background(tcSelBg).Bold(true).MaxWidth(w).Render(row)
	}

	var icon string
	switch it.kind {
	case kAgent:
		icon = tStatusDot(it.status)
	case kSource:
		icon = lipgloss.NewStyle().Foreground(tcDim).Render("@")
	default:
		icon = lipgloss.NewStyle().Foreground(tcDim).Render("#")
	}
	count := ""
	if it.unread > 0 {
		count = lipgloss.NewStyle().Foreground(tcAccent).Bold(true).Render(fmt.Sprintf("(%d) ", it.unread))
	}
	row := tPadRow(marker+icon+" "+it.label, count, w)
	return lipgloss.NewStyle().MaxWidth(w).Render(row)
}

// statusGlyph is the uncoloured status marker, for rows that carry their own
// emphasis (the reverse-video selected row) instead of a coloured dot.
func statusGlyph(it tItem) string {
	switch it.kind {
	case kPipe:
		return "#"
	case kSource:
		return "@"
	}
	switch it.status {
	case "online":
		return "●"
	case "stale":
		return "◐"
	default:
		return "○"
	}
}

func (m tuiModel) addRow() string {
	if m.adding {
		return " " + m.addTi.View()
	}
	return lipgloss.NewStyle().Foreground(tcDim).Render(" [+ add pipe]")
}

func (m tuiModel) renderChat(w, h int) string {
	inner := w - 2
	title := "no conversation"
	if m.count() > 0 {
		title = tChatTitle(m.flatItem(m.sel))
	}
	body := m.vp.View() // scrollable, height == vp.Height == h-6
	titleLine := lipgloss.NewStyle().Bold(true).Foreground(tcAccent).MaxWidth(inner).Render(title)
	sep := lipgloss.NewStyle().Foreground(tcBorder).Render(strings.Repeat("─", inner))
	input := lipgloss.NewStyle().Foreground(tcDim).Render("press enter to reply")
	if m.focus == fChat {
		input = m.chatTi.View()
	}
	content := lipgloss.JoinVertical(lipgloss.Left, titleLine, sep, body, sep, input)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tBorderColor(m.focus == fChat)).
		Width(inner).Height(h - 2)
	return box.Render(content)
}

// chatSenderCol is the sender column's pad width. Bodies align across messages
// at time+2+chatSenderCol+2; a longer handle overflows the column (that one
// message's body sits further right) rather than being truncated.
const chatSenderCol = 8

// wrapMessages renders the full chat log to width `w` with a hanging indent:
// each message is "HH:MM  <sender>  <body>", the body wrapped to the pane and
// every continuation line (from an embedded newline or a wrap) indented to the
// body column with no time/sender — so multi-line payloads read as aligned
// blocks (the readability `ppz read` has). A blank row separates messages.
// Colour lives only on the header (dim time, bold sender); the body is
// default-foreground and control/ANSI-stripped. The viewport clips and scrolls
// the result, so this returns the complete, un-truncated content.
func wrapMessages(msgs []tMsg, w int) string {
	var out []string
	for i, mm := range msgs {
		if i > 0 {
			out = append(out, "") // blank separator between messages
		}
		out = append(out, chatMessageRows(mm, w)...)
	}
	return strings.Join(out, "\n")
}

func chatMessageRows(mm tMsg, w int) []string {
	if w < 1 {
		w = 1
	}
	// Sender name in the header-accent colour (cyan), matching the chat title.
	name, nameColor := mm.sender, tcAccent
	if mm.you {
		name = "you"
	}

	senderW := chatSenderCol
	if lipgloss.Width(name) > senderW {
		senderW = lipgloss.Width(name)
	}
	prefixWidth := lipgloss.Width(mm.t) + 2 + senderW + 2
	indent := strings.Repeat(" ", prefixWidth)

	pad := senderW - lipgloss.Width(name)
	if pad < 0 {
		pad = 0
	}
	header := lipgloss.NewStyle().Foreground(tcDim).Render(mm.t) + "  " +
		lipgloss.NewStyle().Foreground(nameColor).Bold(true).Render(name) +
		strings.Repeat(" ", pad) + "  "

	bodyWidth := w - prefixWidth
	if bodyWidth < 1 {
		bodyWidth = 1
	}
	var body []string
	for _, logical := range strings.Split(cleanText(mm.text), "\n") {
		wrapped := lipgloss.NewStyle().Width(bodyWidth).Render(logical)
		body = append(body, strings.Split(wrapped, "\n")...)
	}
	if len(body) == 0 {
		body = []string{""}
	}

	rows := make([]string, 0, len(body))
	for i, b := range body {
		if i == 0 {
			rows = append(rows, header+b)
		} else {
			rows = append(rows, indent+b)
		}
	}
	return rows
}

// selKey uniquely identifies the selected conversation (kind + key) so the
// viewport can tell a genuine conversation switch from an in-place append.
func selKey(it tItem) string { return fmt.Sprintf("%d:%s", it.kind, it.key) }

// refreshViewport reloads the chat body for the current selection. On a
// conversation switch (or when already scrolled to the bottom) it jumps to the
// newest message; if the reader has scrolled up within the SAME conversation,
// their position is preserved when new messages arrive.
func (m *tuiModel) refreshViewport() {
	if !m.vpReady {
		return
	}
	var content, key string
	if m.count() > 0 {
		it := m.flatItem(m.sel)
		key = selKey(it)
		content = wrapMessages(it.msgs, m.vp.Width)
	}
	switched := key != m.vpKey
	stick := switched || m.vp.AtBottom()
	m.vp.SetContent(content)
	m.vpKey = key
	if stick {
		m.vp.GotoBottom()
	}
}

// cleanText makes an arbitrary message payload safe to render inside the TUI:
// tabs become spaces, and other control characters (CR, and crucially ESC —
// so a payload can't inject ANSI cursor moves / colours that corrupt the
// layout) are dropped. Newlines are preserved; the wrapper re-flows them.
func cleanText(s string) string {
	s = ansi.Strip(s) // remove ANSI escape sequences wholesale
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune('\n')
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20 || r == 0x7f:
			// drop other C0 controls (ESC, CR, BEL, …)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m tuiModel) titleBar() string {
	online := 0
	for _, a := range m.agents {
		if a.status == "online" {
			online++
		}
	}
	left := lipgloss.NewStyle().Bold(true).Foreground(tcAccent).Render(" ppz") +
		lipgloss.NewStyle().Foreground(tcDim).Render(" · "+m.me)
	right := lipgloss.NewStyle().Foreground(tcDim).
		Render(fmt.Sprintf("%d online · %d agents · %d pipes ", online, len(m.agents), len(m.pipes)))
	return tPadRow(left, right, m.w)
}

func (m tuiModel) helpBar() string {
	if m.toast != "" {
		return lipgloss.NewStyle().Foreground(tcErr).MaxWidth(m.w).Render(" " + m.toast)
	}
	var s string
	switch {
	case m.adding:
		s = "type pipe name · enter add · esc cancel"
	case m.focus == fChat:
		s = "type to reply · enter send · pgup/pgdn or scroll · esc/← back · ctrl+c quit"
	default:
		s = "↑/↓ move · enter open · a add · - remove pipe · pgup/pgdn or scroll · q quit"
	}
	return lipgloss.NewStyle().Foreground(tcDim).MaxWidth(m.w).Render(" " + s)
}

// ---- small view helpers ----------------------------------------------------

func tSectionHeader(s string) string {
	return lipgloss.NewStyle().Foreground(tcDim).Bold(true).Render(" " + s)
}

func tStatusDot(status string) string {
	c, r := tcOffline, "○"
	switch status {
	case "online":
		c, r = tcOnline, "●"
	case "stale":
		c, r = tcStale, "◐"
	}
	return lipgloss.NewStyle().Foreground(c).Render(r)
}

func tChatTitle(it tItem) string {
	switch it.kind {
	case kAgent:
		st := it.status
		if st == "" {
			st = "—"
		}
		if it.state != "" {
			st += "|" + it.state
		}
		return fmt.Sprintf("%s · dm · %s", it.label, st)
	case kSource:
		return fmt.Sprintf("%s · dm · inbox", it.label)
	}
	return fmt.Sprintf("#%s · pipe (uncollared)", it.label)
}

func tBorderColor(focused bool) lipgloss.Color {
	if focused {
		return tcBorderF
	}
	return tcBorder
}

func tPadRow(left, right string, w int) string {
	space := w - lipgloss.Width(left) - lipgloss.Width(right)
	if space < 1 {
		space = 1
	}
	return left + strings.Repeat(" ", space) + right
}

func nowHM() string { return time.Now().Format("15:04") }

func hm(created string) string {
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		return t.Local().Format("15:04")
	}
	return nowHM()
}

// ---- request builders (mirror cmdSend / cmdRead target parsing) ------------

// buildSend mirrors cmdSend: a bare leaf (no dot) targets <leaf>.inbox with
// BareTarget set so the daemon falls back to uncollared-pipe resolution when
// the leaf isn't a known source handle; a dotted form is collared handle.pipe.
func buildSend(target, text, session, me string) cliproto.SendRequest {
	if strings.Contains(target, ".") {
		i := strings.LastIndex(target, ".")
		return cliproto.SendRequest{Handle: target[:i], Channel: target[i+1:], Payload: text, Session: session, Sender: me}
	}
	return cliproto.SendRequest{Handle: target, Channel: "inbox", BareTarget: target, Payload: text, Session: session, Sender: me}
}

// buildFollowReq mirrors cmdRead: a bare leaf reads the uncollared pipe
// directly (BareTarget), a dotted form reads collared handle.pipe.
func buildFollowReq(target, session string) cliproto.ReadRequest {
	if strings.Contains(target, ".") {
		i := strings.LastIndex(target, ".")
		return cliproto.ReadRequest{Handle: target[:i], Channel: target[i+1:], Follow: true, Session: session}
	}
	return cliproto.ReadRequest{BareTarget: target, Follow: true, Session: session}
}

// ---- background goroutines -------------------------------------------------

func whoPoller(ctx context.Context, sock string, ch chan tea.Msg) {
	poll := func() {
		var reply cliproto.WhoReply
		if err := daemon.Call(sock, cliproto.IPCWho, cliproto.WhoRequest{}, &reply); err != nil {
			emit(ctx, ch, streamErrMsg{"who", err.Error()})
			return
		}
		emit(ctx, ch, whoMsg{reply.Entries})
	}
	poll()
	t := time.NewTicker(2500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			poll()
		case <-ctx.Done():
			return
		}
	}
}

// sourcePoller refreshes the INBOXES section: it lists sources (same call as
// `ppz ls`) and pushes the message-kind ones. Slower cadence than who — sources
// change rarely (created/destroyed), not every heartbeat.
func sourcePoller(ctx context.Context, sock, session string, ch chan tea.Msg) {
	poll := func() {
		var lr cliproto.ListReply
		if err := daemon.Call(sock, cliproto.IPCList, cliproto.ListRequest{Session: session}, &lr); err != nil {
			return
		}
		emit(ctx, ch, sourcesMsg{lr.Sources})
	}
	poll()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			poll()
		case <-ctx.Done():
			return
		}
	}
}

// streamRead opens a Read{Follow} stream and pushes each message through mk.
// It reconnects with a short backoff until ctx is cancelled (survives daemon
// NATS swaps / restarts, mirroring how `ppz read --tail` is expected to be
// re-run).
func streamRead(ctx context.Context, sock string, req cliproto.ReadRequest, mk func(cliproto.ReadMessage) tea.Msg, ch chan tea.Msg) {
	body, _ := json.Marshal(req)
	scope := followScope(req)
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			emit(ctx, ch, streamErrMsg{scope, err.Error()})
			if sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		_ = json.NewEncoder(conn).Encode(map[string]any{"method": cliproto.IPCRead, "params": json.RawMessage(body)})
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
			case <-done:
			}
			_ = conn.Close()
		}()
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var evt cliproto.ReadEvent
			if json.Unmarshal(sc.Bytes(), &evt) != nil {
				continue
			}
			if evt.Error != nil {
				emit(ctx, ch, streamErrMsg{scope, evt.Error.Error()})
				break // daemon closed the stream; reconnect after backoff
			}
			if evt.Message == nil {
				continue
			}
			emit(ctx, ch, mk(*evt.Message))
		}
		close(done)
		_ = conn.Close()
		if ctx.Err() != nil {
			return
		}
		if sleepCtx(ctx, 2*time.Second) {
			return
		}
	}
}

func followScope(req cliproto.ReadRequest) string {
	if req.BareTarget != "" {
		return req.BareTarget
	}
	return req.Handle + "." + req.Channel
}

func emit(ctx context.Context, ch chan tea.Msg, m tea.Msg) {
	select {
	case ch <- m:
	case <-ctx.Done():
	}
}

func sleepCtx(ctx context.Context, d time.Duration) (cancelled bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-ctx.Done():
		return true
	}
}

// ---- entrypoint ------------------------------------------------------------

// forceColorProfile corrects lipgloss's colour profile for terminals that
// advertise truecolor via $COLORTERM but can't actually parse 24-bit escapes.
// Terminal.app is the classic offender: many shells export COLORTERM=truecolor
// unconditionally, so lipgloss would emit \x1b[38;2;r;g;bm, which Terminal.app
// mis-reads (the r;g;b digits become stray SGR codes → grey boxes). Pinning it
// to ANSI256 makes lipgloss degrade the hex palette to 256-colour, which
// Terminal.app renders correctly. Truecolor terminals (Ghostty, herdr's VT)
// are left untouched and keep the full palette.
func forceColorProfile() {
	if termenv.EnvNoColor() {
		return // honour NO_COLOR / CLICOLOR=0
	}
	if os.Getenv("TERM_PROGRAM") == "Apple_Terminal" {
		lipgloss.SetColorProfile(termenv.ANSI256)
	}
}

func cmdChat(args []string) error {
	if wantsHelp(args) {
		fmt.Fprintln(os.Stdout, "ppz chat — live roster (streaming `ppz who`) + per-agent/-pipe chat.\n\n"+
			"Keys:  ↑/↓ move · enter open · a add pipe · - remove pipe · esc/← back · q quit\n"+
			"Agent DMs stitch your sends to <handle>.inbox with their replies to your inbox.\n"+
			"Give it a stable $PPZ_SESSION if you don't want it sharing a read cursor with a CLI `ppz read`.")
		return nil
	}

	forceColorProfile()

	me, err := effectiveCurrentHandle()
	if err != nil {
		return fmt.Errorf("ppz chat needs a current handle — run `ppz set handle <handle>` or start inside a wrapped agent: %w", err)
	}
	sock := ipcSocket()
	session := sessionID()

	// Preflight — errors on the plain terminal if the daemon's down, instead of
	// behind the alt-screen. Also yields the account id, which scopes the chat
	// store so a different org/server doesn't see this account's history.
	var st cliproto.StatusReply
	if err := daemon.Call(sock, cliproto.IPCStatus, cliproto.StatusRequest{Session: session}, &st); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan tea.Msg, 256)

	go whoPoller(ctx, sock, events)
	go sourcePoller(ctx, sock, session, events)
	go streamRead(ctx, sock,
		cliproto.ReadRequest{Handle: me, Channel: "inbox", Follow: true, Session: session, Sender: me},
		func(rm cliproto.ReadMessage) tea.Msg { return inboundMsg{rm} }, events)

	// Durable chat store, scoped by account + identity so it's isolated across
	// orgs/servers and survives restarts/terminals.
	store, err := chatstore.OpenForAccount(home(), st.AccountID, me)
	if err != nil {
		return err
	}

	m := newTUIModel(me, session, sock, events, ctx)
	m.store = store
	m.hydrate() // render history + read state immediately; the follow reconciles

	// INBOXES load asynchronously via sourcePoller (started above) so the UI
	// paints immediately rather than blocking on a server round-trip. A source
	// DM that arrives before the first source list lands is routed to AGENTS and
	// migrated to INBOXES on classification (see upsertSource).

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = p.Run()
	_ = store.Flush() // persist on exit (crash falls back to JetStream re-hydrate)
	return err
}
