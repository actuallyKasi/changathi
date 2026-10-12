package cli

import (
	"changathi/internal/core"
	"changathi/internal/service"
	"changathi/internal/transcript"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

type statusMsg struct {
	snapshot core.Snapshot
	err      error
}
type pageMsg struct {
	attempt string
	before  int64
	page    core.EventPage
	err     error
}
type actionMsg struct {
	attempt, text string
	message       core.Message
	err           error
}
type exportMsg struct {
	path string
	err  error
}
type mutationMsg struct {
	label string
	err   error
}
type pulseMsg time.Time
type row struct {
	id, title, detail, attempt string
	subject                    string
	state, issueState          string
	issue                      int
	plan                       *core.Supervisor
}
type sessionView struct {
	events                  []core.Event
	transcript              transcript.Transcript
	cursor, before          int64
	more, gap, busy, loaded bool
	next                    time.Time
	failures                int
	offset                  int
	follow                  bool
	unread                  int
	draft                   string
	expanded                map[string]bool
	pending                 *service.Request
	feedID                  string
	loadError               string
}

type tuiModel struct {
	ctx                             context.Context
	client                          *Client
	snapshot                        core.Snapshot
	rows                            []row
	selected                        string
	focused                         string
	width, height                   int
	viewport                        viewport.Model
	composer                        textarea.Model
	search                          textinput.Model
	mode, query, notice, connection string
	dark, plain, raw, help, sidebar bool
	group                           string
	history                         bool
	sessions                        map[string]*sessionView
	renderer                        *transcript.Renderer
	renderWidth                     int
	itemKeys                        []string
	itemOffsets                     []int
	itemCursor                      int
	sending                         bool
	confirm                         *service.Request
	confirmLabel                    string
	drafts                          map[string]string
}

func newTUI(ctx context.Context, client *Client, attempt string) *tuiModel {
	c := textarea.New()
	c.Placeholder = "Message this agent…"
	c.CharLimit = 16384
	c.SetHeight(4)
	s := textinput.New()
	s.Placeholder = "Search transcript"
	s.Prompt = "/ "
	s.CharLimit = 200
	m := &tuiModel{ctx: ctx, client: client, selected: attempt, focused: attempt, width: 100, height: 30,
		viewport: viewport.New(), composer: c, search: s, dark: true,
		plain:   os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb",
		sidebar: attempt == "", sessions: map[string]*sessionView{}, drafts: map[string]string{}, connection: "Connecting…"}
	m.resize()
	return m
}

func (a *app) interactive(ctx context.Context, attempt string) error {
	m := newTUI(ctx, a.client(), attempt)
	m.plain = m.plain || a.plain
	m.renderer = nil
	m.resize()
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithOutput(a.out), tea.WithFPS(30)}
	if m.plain {
		options = append(options, tea.WithColorProfile(colorprofile.NoTTY))
	}
	_, err := tea.NewProgram(m, options...).Run()
	if ctx.Err() != nil || errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(m.fetchStatus(), pulse(), tea.RequestBackgroundColor)
}
func pulse() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return pulseMsg(t) })
}
func (m *tuiModel) fetchStatus() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		s, err := m.client.Status(ctx)
		return statusMsg{s, err}
	}
}
func (m *tuiModel) state(id string) *sessionView {
	s := m.sessions[id]
	if s == nil {
		// Cache only a few agents; returning to an evicted one reloads its history.
		if len(m.sessions) >= 6 {
			for key, old := range m.sessions {
				if key != m.selected && !old.busy && old.pending == nil {
					m.drafts[key] = old.draft
					delete(m.sessions, key)
					break
				}
			}
		}
		s = &sessionView{follow: true, expanded: map[string]bool{}, draft: m.drafts[id]}
		m.sessions[id] = s
	}
	return s
}

func (m *tuiModel) trimResidentHistory(keep string) {
	count := 0
	for _, s := range m.sessions {
		if len(s.events) > 0 {
			count++
		}
	}
	for id, s := range m.sessions {
		if count <= 6 {
			return
		}
		if id == keep || id == m.attempt() || s.busy || len(s.events) == 0 {
			continue
		}
		// Keep drafts and uncertain submissions, but release their history cache.
		s.events = nil
		s.transcript = transcript.Transcript{}
		s.loaded = false
		s.cursor = 0
		s.before = 0
		s.more = false
		s.expanded = map[string]bool{}
		count--
	}
}
func (m *tuiModel) selectedRow() *row {
	for n := range m.rows {
		if m.rows[n].id == m.selected {
			return &m.rows[n]
		}
	}
	return nil
}
func (m *tuiModel) attempt() string {
	if r := m.selectedRow(); r != nil {
		return r.attempt
	}
	return m.focused
}
func (m *tuiModel) fetchPage(before int64) tea.Cmd {
	id := m.attempt()
	if id == "" {
		return nil
	}
	s := m.state(id)
	if s.busy {
		return nil
	}
	s.busy = true
	after := s.cursor
	if before > 0 || !s.loaded {
		after = 0
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		p, err := m.client.Transcript(ctx, id, after, before)
		return pageMsg{id, before, p, err}
	}
}

func (m *tuiModel) updateRows() {
	names := map[string]string{}
	for _, p := range m.snapshot.Projects {
		names[p.ID] = p.Name
	}
	if m.group == "" && len(m.snapshot.Supervisors) > 0 {
		m.group = m.snapshot.Supervisors[0].ID
	}
	rows := []row{}
	owned := map[string]bool{}
	for n := range m.snapshot.Supervisors {
		p := &m.snapshot.Supervisors[n]

		for _, c := range p.Children {
			id := "child:" + p.ID + fmt.Sprint(c.Issue)
			if c.AttemptID != "" {
				id = c.AttemptID
				owned[id] = true
			}
			if m.focused == "" && p.ID != m.group {
				continue
			}
			subject := c.Title
			issueState := ""
			if c.Reason == "issue remains open" {
				issueState = "open"
			}
			if c.State == "completed" {
				issueState = "closed"
			}
			if subject == "" {
				for _, a := range m.snapshot.Agents {
					if a.ProjectID == p.ProjectID && a.Issue.Number == c.Issue {
						subject = a.Issue.Title
						break
					}
				}
			}
			rows = append(rows, row{id: id, issue: c.Issue, title: fmt.Sprintf("  #%d", c.Issue), subject: subject, detail: c.State + " " + c.Reason, state: c.State, issueState: issueState, attempt: c.AttemptID, plan: p})
		}
	}
	agents := append([]core.Attempt(nil), m.snapshot.Agents...)
	sort.SliceStable(agents, func(i, j int) bool {
		if agents[i].Active() != agents[j].Active() {
			return agents[i].Active()
		}
		return agents[i].Created.After(agents[j].Created)
	})
	for _, a := range agents {
		if owned[a.ID] {
			continue
		}
		if m.focused == "" && len(m.snapshot.Supervisors) > 0 {
			if m.group == "standalone" {
				if a.SupervisorID != "" {
					continue
				}
			} else if !m.history || a.SupervisorID != m.group {
				continue
			}
		}
		rows = append(rows, row{id: a.ID, attempt: a.ID, title: fmt.Sprintf("%s#%d", names[a.ProjectID], a.Issue.Number), subject: a.Issue.Title, detail: a.State, state: a.State})
	}
	m.rows = rows
	if m.selectedRow() == nil && m.focused == "" && len(rows) > 0 {
		m.selected = rows[0].id
	}
}

func (m *tuiModel) selectRow(delta int) {
	if len(m.rows) == 0 {
		return
	}
	n := 0
	for i, r := range m.rows {
		if r.id == m.selected {
			n = i
			break
		}
	}
	m.saveScroll()
	n = max(0, min(len(m.rows)-1, n+delta))
	m.selected = m.rows[n].id
	m.itemCursor = 0
	m.query = ""
	m.raw = false
	m.renderer = nil
	m.resize()
	m.refresh(false)
}
func (m *tuiModel) saveScroll() {
	if id := m.attempt(); id != "" {
		s := m.state(id)
		s.offset = m.viewport.YOffset()
	}
}
func (m *tuiModel) sidebarWidth() int {
	return min(64, max(42, m.width*2/5))
}

func (m *tuiModel) supervisorSidebar(width int) string {
	p := m.currentSupervisor()
	if p == nil {
		return ""
	}
	lines := []string{m.style(fmt.Sprintf("SUPERVISOR #%d", p.Parent.Number), false)}
	lines = append(lines, strings.Split(ansi.Hardwrap(transcript.Clean(p.Parent.Title), width, true), "\n")...)
	lines = append(lines, m.statusBadge(p.State))
	if body := strings.TrimSpace(transcript.Clean(p.Parent.Body)); body != "" {
		lines = append(lines, strings.Split(ansi.Hardwrap(body, width, true), "\n")...)
	}
	limit := min(8, max(1, m.viewport.Height()-6))
	if len(lines) > limit {
		lines = lines[:limit]
		lines[limit-1] = ansi.Truncate(lines[limit-1], max(1, width-1), "") + "…"
	}
	return strings.Join(lines, "\n") + "\n\n"
}

func (m *tuiModel) currentSupervisor() *core.Supervisor {
	if m.focused != "" {
		return nil
	}
	for n := range m.snapshot.Supervisors {
		if m.snapshot.Supervisors[n].ID == m.group {
			return &m.snapshot.Supervisors[n]
		}
	}
	return nil
}

func (m *tuiModel) bodyTop() int {
	if m.currentSupervisor() != nil && m.width < 90 {
		return 5
	}
	return 3
}

func (m *tuiModel) resize() {
	w := max(10, m.width-4)
	if m.width >= 90 && m.focused == "" {
		w = max(10, m.width-m.sidebarWidth()-6)
	}
	h := max(1, m.height-m.bodyTop()-4)
	if m.mode == "compose" {
		h = max(1, h-6)
	}
	if m.mode == "search" {
		h = max(1, h-2)
	}
	m.viewport.SetWidth(w)
	m.viewport.SetHeight(h)
	m.composer.SetWidth(w)
	m.search.SetWidth(max(5, w-3))
	if m.renderer == nil || w != m.renderWidth {
		m.renderer = transcript.NewRenderer(w, m.plain, m.dark)
		m.renderWidth = w
	}
}

func (m *tuiModel) refresh(anchor bool) {
	oldOffset := m.viewport.YOffset()
	anchorKey := ""
	inside := 0
	if anchor {
		for n, offset := range m.itemOffsets {
			if offset <= oldOffset {
				anchorKey = m.itemKeys[n]
				inside = oldOffset - offset
			}
		}
	}
	m.itemKeys = nil
	m.itemOffsets = nil
	id := m.attempt()
	if id == "" {
		text := "No agents yet. Register a project and run an issue to get started."
		if m.currentSupervisor() != nil {
			text = "No child issues available for this supervisor yet."
		}
		if r := m.selectedRow(); r != nil && r.plan != nil {
			p := r.plan
			if r.issue == 0 {
				text = fmt.Sprintf("%s\n\n%s\n\n%s\n", p.Parent.Title, ansi.Strip(m.statusBadge(p.State)), p.Reason)
				for _, c := range p.Children {
					text += fmt.Sprintf("#%d  %s\n  %s\n  %s\n", c.Issue, c.Title, ansi.Strip(m.statusBadge(c.State)), c.Reason)
				}
			} else {
				text = fmt.Sprintf("#%d — %s\n\n%s\n\nNo agent transcript is available for this issue yet.\n", r.issue, r.subject, ansi.Strip(m.rowBadges(*r))+"\n"+ansi.Strip(m.agentBadge(*r)))
				for _, child := range p.Children {
					if child.Issue == r.issue && child.Reason != "" {
						text += "\n" + child.Reason + "\n"
					}
				}
				for _, c := range p.Plan.Children {
					if c.Issue != r.issue || len(c.DependsOn) == 0 {
						continue
					}
					text += "\nDepends on:\n"
					for _, dependency := range c.DependsOn {
						label := fmt.Sprintf("#%d", dependency)
						for _, status := range p.Children {
							if status.Issue == dependency {
								label += fmt.Sprintf(" — %s %s", status.Title, ansi.Strip(m.statusBadge(status.State)))
								break
							}
						}
						text += "  " + label + "\n"
					}
				}
			}
		}
		m.viewport.SetContent(ansi.Hardwrap(transcript.Clean(text), m.renderWidth, true))
		return
	}
	s := m.state(id)
	var b strings.Builder
	line := 0
	if s.gap || s.transcript.Trimmed {
		text := "Earlier history is incomplete or outside the viewer limit.\n\n"
		b.WriteString(text)
		line += 2
	}
	if s.more {
		b.WriteString("Older output available · press p to load\n\n")
		line += 2
	}
	if !s.loaded {
		if strings.HasPrefix(s.loadError, "Daemon unavailable") {
			b.WriteString("Waiting to load agent output…\n")
		} else if s.loadError != "" {
			b.WriteString("Unable to load agent output:\n" + s.loadError + "\nRetrying automatically…\n")
		} else {
			b.WriteString("Loading agent output…\n")
		}
	}

	if m.raw {
		for _, e := range s.events {
			data, _ := json.MarshalIndent(e, "", "  ")
			text := transcript.Clean(string(data))
			if m.query != "" && !strings.Contains(strings.ToLower(text), strings.ToLower(m.query)) {
				continue
			}
			b.WriteString(ansi.Hardwrap(text, m.renderWidth, true))
			b.WriteString("\n\n")
		}
	} else {
		visible := []transcript.Item{}
		for _, i := range s.transcript.Items {
			if m.query == "" || strings.Contains(strings.ToLower(i.Title+" "+i.Text+" "+i.Status), strings.ToLower(m.query)) {
				visible = append(visible, i)
			}
		}
		m.itemCursor = max(0, min(m.itemCursor, len(visible)-1))
		for n, i := range visible {
			m.itemKeys = append(m.itemKeys, i.Key)
			m.itemOffsets = append(m.itemOffsets, line)
			expanded, known := s.expanded[i.Key]
			if !known {
				expanded = i.Status == "failed"
			}
			text := m.renderer.Render(i, expanded, !m.sidebar && n == m.itemCursor)
			b.WriteString(text)
			line += strings.Count(text, "\n")
		}
	}
	if s.loaded && b.Len() == 0 {
		b.WriteString("No output matches this view.\n")
	}
	m.viewport.SetContent(b.String())
	if s.follow {
		m.viewport.GotoBottom()
		s.unread = 0
	} else {
		offset := s.offset
		if anchor && anchorKey != "" {
			for n, key := range m.itemKeys {
				if key == anchorKey {
					offset = m.itemOffsets[n] + inside
					break
				}
			}
		}
		m.viewport.SetYOffset(offset)
	}
	s.offset = m.viewport.YOffset()
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.MouseWheelMsg:
		if m.help || m.mode != "" || v.Y < m.bodyTop() || v.Y >= m.bodyTop()+m.viewport.Height() {
			return m, nil
		}
		if v.Button != tea.MouseWheelUp && v.Button != tea.MouseWheelDown {
			return m, nil
		}
		inList := m.focused == "" && ((m.width >= 90 && v.X < m.sidebarWidth()) || (m.width < 90 && m.sidebar))
		if inList {
			if m.width >= 90 && v.Y < m.bodyTop()+strings.Count(m.supervisorSidebar(m.sidebarWidth()-1), "\n") {
				return m, nil
			}
			delta := 1
			if v.Button == tea.MouseWheelUp {
				delta = -1
			}
			m.sidebar = true
			m.selectRow(delta)
			return m, m.fetchPage(0)
		}
		m.sidebar = false
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		if id := m.attempt(); id != "" {
			s := m.state(id)
			s.follow = m.viewport.AtBottom()
			s.offset = m.viewport.YOffset()
			if s.follow {
				s.unread = 0
			}
		}
		return m, cmd
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		m.resize()
		m.refresh(true)
	case tea.BackgroundColorMsg:
		m.dark = v.IsDark()
		m.renderer = nil
		m.resize()
		m.refresh(true)
	case statusMsg:
		if v.err != nil {
			m.connection = viewerError(v.err)
			m.refresh(true)
		} else {
			m.connection = "Connected"
			m.snapshot = v.snapshot
			m.updateRows()
			m.resize()
			if id := m.attempt(); id != "" {
				m.state(id).transcript.Messages(m.snapshot.Messages, id)
			}
			m.refresh(true)
		}
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return statusRefreshMsg{} })
	case statusRefreshMsg:
		return m, m.fetchStatus()
	case pulseMsg:
		var cmd tea.Cmd
		if id := m.attempt(); id != "" {
			s := m.state(id)
			if !s.busy && time.Now().After(s.next) {
				cmd = m.fetchPage(0)
			}
		}
		return m, tea.Batch(pulse(), cmd)
	case pageMsg:
		s := m.state(v.attempt)
		s.busy = false
		if v.err != nil {
			s.loadError = viewerError(v.err)
			var response *ResponseError
			if errors.As(v.err, &response) && response.Message == "unknown API route" {
				s.loadError = "Restart the daemon to enable the transcript API."
			}
			s.failures++
			s.next = time.Now().Add(time.Duration(min(10, s.failures)) * time.Second)
			if v.attempt == m.attempt() {
				m.notice = transcript.Clean(v.err.Error())
				m.refresh(true)
			}
			return m, nil
		}
		s.failures = 0
		s.loadError = ""
		s.next = time.Now().Add(250 * time.Millisecond)
		if s.feedID != "" && v.page.FeedID != s.feedID {
			s.feedID = v.page.FeedID
			s.cursor = 0
			s.before = 0
			s.events = nil
			s.loaded = false
			s.gap = true
			s.more = false
			s.transcript = transcript.Transcript{}
			if v.attempt == m.attempt() {
				m.renderer = nil
				m.resize()
				m.refresh(false)
			}
			return m, nil
		}
		s.feedID = v.page.FeedID
		if v.before == 0 {
			if !s.loaded || v.page.Gap {
				s.more = v.page.HasMore
				s.before = v.page.Before
			}
			if v.page.Gap && s.loaded {
				s.events = nil
				s.transcript = transcript.Transcript{}
				s.gap = true
			}
			s.cursor = v.page.Cursor
		} else {
			s.more = v.page.HasMore
			s.before = v.page.Before
		}
		s.gap = s.gap || v.page.Gap
		s.loaded = true
		if len(v.page.Events) > 0 {
			s.events = append(s.events, v.page.Events...)
			sort.Slice(s.events, func(i, j int) bool { return s.events[i].Seq < s.events[j].Seq })
			unique := s.events[:0]
			var last int64
			for _, e := range s.events {
				if e.Seq > last {
					unique = append(unique, e)
					last = e.Seq
				}
			}
			s.events = unique
			bytes := 0
			for _, e := range s.events {
				bytes += len(e.Text)
				if e.Output != nil {
					bytes += len(e.Output.Text) + len(e.Output.Title)
				}
			}
			for len(s.events) > 2000 || bytes > 4*1024*1024 {
				e := s.events[0]
				bytes -= len(e.Text)
				if e.Output != nil {
					bytes -= len(e.Output.Text) + len(e.Output.Title)
				}
				s.events = s.events[1:]
				s.gap = true
			}
			if v.before > 0 {
				s.transcript = transcript.Transcript{}
				for _, e := range s.events {
					s.transcript.Apply(e)
				}
			} else {
				for _, e := range v.page.Events {
					s.transcript.Apply(e)
				}
			}
			if !s.follow && v.before == 0 {
				s.unread += len(v.page.Events)
			}
		}
		s.transcript.Messages(m.snapshot.Messages, v.attempt)
		m.trimResidentHistory(v.attempt)
		if v.attempt == m.attempt() {
			if v.before > 0 || v.page.Gap {
				m.renderer = nil
				m.resize()
			}
			m.refresh(true)
		}
	case actionMsg:
		m.sending = false
		if v.err != nil {
			var rejected *ResponseError
			if errors.As(v.err, &rejected) {
				m.state(v.attempt).pending = nil
			}
			m.notice = "Message failed or outcome uncertain: " + transcript.Clean(v.err.Error())
		} else {
			m.snapshot.Messages = append(m.snapshot.Messages, v.message)
			s := m.state(v.attempt)
			s.pending = nil
			if s.draft == v.text {
				s.draft = ""
				delete(m.drafts, v.attempt)
			}
			if m.attempt() == v.attempt && m.mode == "compose" && m.composer.Value() == v.text {
				m.composer.Reset()
				m.composer.Blur()
				m.mode = ""
			}
			m.notice = "Message " + v.message.State
			m.state(v.attempt).transcript.Messages(m.snapshot.Messages, v.attempt)
			m.resize()
			m.refresh(true)
		}
	case exportMsg:
		if v.err != nil {
			m.notice = transcript.Clean(v.err.Error())
		} else {
			m.notice = "Exported to " + v.path
		}
	case mutationMsg:
		if v.err != nil {
			m.notice = transcript.Clean(v.err.Error())
		} else {
			m.notice = v.label + " requested"
		}
	case tea.KeyPressMsg:
		key := v.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.mode == "confirm" {
			if key == "esc" {
				m.confirm = nil
				m.mode = ""
				m.notice = "Action cancelled"
				return m, nil
			}
			if key == "enter" && m.confirm != nil {
				req, label := *m.confirm, m.confirmLabel
				m.confirm = nil
				m.mode = ""
				return m, func() tea.Msg { err := m.client.Action(m.ctx, req, nil); return mutationMsg{label, err} }
			}
			return m, nil
		}
		if m.mode == "compose" {
			if key == "esc" {
				m.state(m.attempt()).draft = m.composer.Value()
				m.composer.Blur()
				m.mode = ""
				m.resize()
				m.refresh(true)
				return m, nil
			}
			if key == "ctrl+s" && !m.sending && strings.TrimSpace(m.composer.Value()) != "" {
				if reason := m.messageBlockedReason(); reason != "" {
					m.state(m.attempt()).draft = m.composer.Value()
					m.notice = reason + " · Draft saved"
					return m, nil
				}
				m.sending = true
				id, text := m.attempt(), m.composer.Value()
				req := service.Request{ID: core.ID(), Action: "agent.message", Attempt: id, Text: text}
				s := m.state(id)
				s.draft = text
				if s.pending != nil {
					if s.pending.Text != text {
						m.sending = false
						m.notice = "Previous message outcome is uncertain; inspect its delivery before changing and resending"
						return m, nil
					}
					req = *s.pending
				} else {
					s.pending = &req
				}
				return m, func() tea.Msg {
					var message core.Message
					err := m.client.Action(m.ctx, req, &message)
					return actionMsg{id, text, message, err}
				}
			}
			var cmd tea.Cmd
			m.composer, cmd = m.composer.Update(msg)
			return m, cmd
		}
		if m.mode == "search" {
			if key == "esc" || key == "enter" {
				if key == "esc" {
					m.query = ""
				}
				m.search.Blur()
				m.mode = ""
				m.resize()
				m.refresh(false)
				return m, nil
			}
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.query = m.search.Value()
			m.refresh(false)
			return m, cmd
		}
		if m.help {
			m.help = false
			return m, nil
		}
		switch key {
		case "q":
			return m, tea.Quit
		case "?":
			m.help = true
		case "s", "t":
			if id := m.attempt(); id != "" {
				for _, a := range m.snapshot.Agents {
					if a.ID != id {
						continue
					}
					req := service.Request{ID: core.ID(), Action: "agent.stop", Attempt: id}
					label := "Stop " + id
					if key == "t" {
						req.Action = "issue.retry"
						req.Number = a.Issue.Number
						req.Project = a.ProjectID
						req.Attempt = ""
						label = fmt.Sprintf("Retry issue #%d in project %s", a.Issue.Number, a.ProjectID)
					}
					m.confirm = &req
					m.confirmLabel = label
					m.mode = "confirm"
					m.notice = label + "? Enter confirms · Esc cancels"
					break
				}
			}
		case "left", "right", "h":
			if m.focused != "" {
				return m, nil
			}
			m.saveScroll()
			if key == "h" {
				m.history = !m.history
			} else {
				groups := []string{}
				for _, p := range m.snapshot.Supervisors {
					groups = append(groups, p.ID)
				}
				groups = append(groups, "standalone")
				idx := 0
				for n, id := range groups {
					if id == m.group {
						idx = n
					}
				}
				delta := 1
				if key == "left" {
					delta = -1
				}
				m.group = groups[(idx+delta+len(groups))%len(groups)]
			}
			m.updateRows()
			m.renderer = nil
			m.resize()
			m.refresh(false)
			return m, m.fetchPage(0)
		case "tab":
			m.sidebar = !m.sidebar
			m.refresh(true)
		case "esc":
			m.query = ""
			m.sidebar = m.focused == ""
			m.refresh(true)
		case "m":
			if reason := m.messageBlockedReason(); reason != "" {
				m.notice = reason
				return m, nil
			}
			if id := m.attempt(); id != "" {
				m.sidebar = false
				m.mode = "compose"
				m.composer.SetValue(m.state(id).draft)
				m.resize()
				m.refresh(true)
				return m, m.composer.Focus()
			}
		case "/":
			m.sidebar = false
			m.mode = "search"
			m.search.SetValue(m.query)
			m.resize()
			return m, m.search.Focus()
		case "r":
			m.raw = !m.raw
			m.refresh(true)
		case "e":
			if id := m.attempt(); id != "" {
				text := m.state(id).transcript.Export()
				return m, func() tea.Msg {
					f, err := os.CreateTemp("", "changathi-transcript-*.md")
					if err != nil {
						return exportMsg{err: err}
					}
					path := f.Name()
					_, err = f.WriteString(text)
					closeErr := f.Close()
					if err == nil {
						err = closeErr
					}
					return exportMsg{path, err}
				}
			}
		case "p":
			if id := m.attempt(); id != "" {
				s := m.state(id)
				if s.more {
					s.follow = false
					return m, m.fetchPage(s.before)
				}
			}
		case "end", "G", "f":
			if id := m.attempt(); id != "" {
				m.state(id).follow = true
				m.refresh(false)
			}
		case "[", "]":
			if len(m.itemOffsets) > 0 {
				m.sidebar = false
				if key == "[" {
					m.itemCursor--
				} else {
					m.itemCursor++
				}
				m.itemCursor = max(0, min(m.itemCursor, len(m.itemOffsets)-1))
				if id := m.attempt(); id != "" {
					m.state(id).follow = false
				}
				m.refresh(true)
				m.viewport.SetYOffset(m.itemOffsets[m.itemCursor])
				m.saveScroll()
			}
		case "enter":
			if m.sidebar {
				m.sidebar = false
				m.refresh(true)
			} else if id := m.attempt(); id != "" && len(m.itemKeys) > 0 {
				s := m.state(id)
				key := m.itemKeys[m.itemCursor]
				expanded, known := s.expanded[key]
				if !known {
					for _, i := range s.transcript.Items {
						if i.Key == key {
							expanded = i.Status == "failed"
						}
					}
				}
				s.expanded[key] = !expanded
				m.refresh(true)
			}
		case "up", "k", "down", "j":
			if m.sidebar {
				delta := 1
				if key == "up" || key == "k" {
					delta = -1
				}
				m.selectRow(delta)
				return m, m.fetchPage(0)
			}
			fallthrough
		default:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			if id := m.attempt(); id != "" {
				s := m.state(id)
				s.follow = m.viewport.AtBottom()
				s.offset = m.viewport.YOffset()
				if s.follow {
					s.unread = 0
				}
			}
			return m, cmd
		}
	}
	return m, nil
}

type statusRefreshMsg struct{}

func (m *tuiModel) messageBlockedReason() string {
	if m.attempt() == "" {
		return "No agent to message for this issue"
	}
	if m.connection != "Connected" {
		return "Messaging unavailable until the daemon reconnects"
	}
	for _, a := range m.snapshot.Agents {
		if a.ID != m.attempt() {
			continue
		}
		switch a.State {
		case "running", "preparing":
			return ""
		case "validating", "pr_open", "merging":
			return "Agent is delivering its work and cannot accept messages"
		default:
			return "Agent is inactive and cannot accept messages"
		}
	}
	return "Agent status is unknown; messaging unavailable"
}

func viewerError(err error) string {
	var transport *url.Error
	if errors.As(err, &transport) {
		return "Daemon unavailable · retrying · run: changathi daemon start"
	}
	return transcript.Clean(err.Error())
}

func (m *tuiModel) statusBadge(state string) string {
	if state == "" {
		return ""
	}
	icon, dark, light := "•", "#A9B2C6", "#525866"
	label := transcript.Clean(state)
	switch state {
	case "done", "completed":
		icon, dark, light = "✓", "#86EFAC", "#166534"
	case "open":
		icon, dark, light = "○", "#86EFAC", "#166534"
	case "closed":
		icon, dark, light = "●", "#C4B5FD", "#6B21A8"
	case "pending", "queued", "retry_wait", "waiting_account", "waiting_checks", "waiting_capacity", "waiting_remote", "waiting_child":
		icon, dark, light = "◷", "#FDE68A", "#854D0E"
	case "superseded":
		icon = "↪"
	case "running", "active", "starting", "preparing", "validating", "pr_open", "merging", "agent_active":
		icon, dark, light = "▶", "#93C5FD", "#1E40AF"
	case "awaiting_review", "waiting_review", "awaiting_parent_review":
		icon, dark, light = "◇", "#C4B5FD", "#6B21A8"
	case "failed", "needs_attention":
		icon, dark, light = "!", "#FCA5A5", "#991B1B"
	case "paused", "cancelled":
		icon = "Ⅱ"
	}
	label = transcript.StateLabel(label)
	text := "[" + icon + " " + label + "]"
	if m.plain {
		return text
	}
	color, background := dark, "#202838"
	if !m.dark {
		color, background = light, "#EEF0F5"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Background(lipgloss.Color(background)).Render(text)
}

// Activity describes execution now, not whether an attempt remains eligible to run.
func (m *tuiModel) agentBadge(r row) string {
	if r.plan != nil && r.issue == 0 {
		return ""
	}
	if r.attempt == "" {
		return m.statusBadge("no_agent")
	}
	if m.connection != "Connected" {
		return m.statusBadge("agent_unknown")
	}
	for _, a := range m.snapshot.Agents {
		if a.ID != r.attempt {
			continue
		}
		switch a.State {
		case "preparing", "running", "validating", "pr_open", "merging":
			return m.statusBadge("agent_active")
		default:
			return m.statusBadge("agent_inactive")
		}
	}
	return m.statusBadge("agent_unknown")
}

func (m *tuiModel) rowBadges(r row) string {
	badges := m.statusBadge(r.state)
	if r.issueState != "" && r.issueState != r.state {
		badges += " " + m.statusBadge(r.issueState)
	}
	return badges
}

func (m *tuiModel) style(text string, muted bool) string {
	if m.plain {
		return text
	}
	c := "#A9B2C6"
	if !muted {
		c = "#8B9EFF"
	}
	if !m.dark {
		c = "#4338CA"
		if muted {
			c = "#525866"
		}
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(text)
}
func (m *tuiModel) View() tea.View {
	width := max(10, m.width)
	header := "CHANGATHI"
	if r := m.selectedRow(); r != nil {
		header = "CHANGATHI  /  " + r.title
	}
	if m.focused != "" && m.selectedRow() == nil {
		header = "CHANGATHI  /  " + m.focused
	}
	sub := "Agent output"
	if m.sidebar {
		sub = "Issues ↑/↓ · ←/→ supervisor · h history"
		if m.history {
			sub += " on"
		}
	}
	if id := m.attempt(); id != "" {
		for _, a := range m.snapshot.Agents {
			if a.ID == id {
				sub += "  ·  Codex"
				if a.Workflow.Model != "" {
					sub += " / " + transcript.Clean(a.Workflow.Model)
				}
				break
			}
		}
		s := m.state(id)
		if s.follow {
			sub += "  ·  Auto-scroll on"
		} else {
			sub += fmt.Sprintf("  ·  Scroll paused · %d new events", s.unread)
		}
	}
	if m.query != "" {
		sub += "  ·  filter: " + m.query
	}
	if m.raw {
		sub += "  ·  raw events"
	}
	body := m.viewport.View()
	if m.width >= 90 && m.focused == "" {
		sideWidth := m.sidebarWidth()
		var side strings.Builder
		supervisor := m.supervisorSidebar(sideWidth - 1)
		side.WriteString(supervisor)
		selected := 0
		for n, r := range m.rows {
			if r.id == m.selected {
				selected = n
			}
		}
		capacity := max(1, (m.viewport.Height()-strings.Count(supervisor, "\n"))/5)
		start := max(0, selected-capacity+1)
		for _, r := range m.rows[start:min(len(m.rows), start+capacity)] {
			prefix := "  "
			if r.id == m.selected {
				prefix = "> "
			}
			side.WriteString(ansi.Truncate(m.style(prefix+transcript.Clean(r.title), r.id != m.selected)+"  "+m.statusBadge(r.issueState), sideWidth-1, "…") + "\n")
			side.WriteString(ansi.Truncate("  "+strings.Join(strings.Fields(transcript.Clean(r.subject)), " "), sideWidth-1, "…") + "\n")
			side.WriteString(ansi.Truncate("  "+m.statusBadge(r.state), sideWidth-1, "…") + "\n")
			side.WriteString("  " + m.agentBadge(r) + "\n\n")
		}
		if len(m.rows) == 0 {
			side.WriteString("No child issues or agents\n")
		}
		left := lipgloss.NewStyle().Width(sideWidth).Height(m.viewport.Height()).Render(side.String())
		divider := " │ " + strings.Repeat("\n │ ", max(0, m.viewport.Height()-1))
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, m.style(divider, true), body)
	} else if m.sidebar && m.focused == "" {
		var b strings.Builder
		selected := 0
		for n, r := range m.rows {
			if r.id == m.selected {
				selected = n
			}
		}
		capacity := max(1, m.viewport.Height()/5)
		start := max(0, selected-capacity+1)
		for _, r := range m.rows[start:min(len(m.rows), start+capacity)] {
			prefix := "  "
			if r.id == m.selected {
				prefix = "> "
			}
			b.WriteString(ansi.Truncate(prefix+transcript.Clean(r.title)+"  "+m.statusBadge(r.issueState), width-2, "…") + "\n" + ansi.Truncate("  "+strings.Join(strings.Fields(transcript.Clean(r.subject)), " "), width-2, "…") + "\n" + ansi.Truncate("  "+m.statusBadge(r.state), width-2, "…") + "\n" + ansi.Truncate("  "+m.agentBadge(r), width-2, "…") + "\n\n")
		}
		body = lipgloss.NewStyle().Height(m.viewport.Height()).Render(b.String())
	}
	if m.help {
		body = fmt.Sprintf("%s\n\nTab       switch agents / transcript\n←/→       switch supervisor / standalone agents\nh         show/hide previous attempts\n↑/↓ j/k   navigate or scroll\nEnter     open agent / expand selected tool\n[ / ]     previous / next transcript item\n/         search transcript · Esc clears\np         load older output\nf / End   follow live output\nm         compose message · Ctrl-S sends · Esc saves draft\nr         toggle raw events\ne         export transcript as Markdown\ns / t     stop agent / retry issue (Enter confirms)\nq         quit viewer (agents keep running)\n\nPress any key to return.", m.style("Keyboard shortcuts", false))
	}
	if m.mode == "compose" {
		body += "\n" + m.style("Message agent · Ctrl-S send · Esc save draft", false) + "\n" + m.composer.View()
	}
	if m.mode == "search" {
		body += "\n" + m.search.View()
	}
	footer := "Tab focus · / search · m message · ? help · q quit"
	if m.sending {
		footer = "Sending message…"
	}
	notice := m.notice
	if m.connection != "Connected" {
		notice = m.connection
	}
	supervisorHeader := ""
	if p := m.currentSupervisor(); p != nil && m.width < 90 {
		supervisorHeader = ansi.Truncate(m.style(fmt.Sprintf("SUPERVISOR #%d", p.Parent.Number), false)+"  "+m.statusBadge(p.State), width, "…") + "\n" + ansi.Truncate(transcript.Clean(p.Parent.Title), width, "…") + "\n"
	}
	content := supervisorHeader + ansi.Truncate(m.style(transcript.Clean(header), false), width, "…") + "\n" + m.style(ansi.Truncate(sub, width, "…"), true) + "\n\n" + body + "\n" + m.style(ansi.Truncate(transcript.Clean(notice), width, "…"), true) + "\n" + m.style(ansi.Truncate(footer, width, "…"), true)
	// Clip to the actual terminal dimensions, including very small windows.
	lines := strings.Split(content, "\n")
	if len(lines) > m.height {
		lines = lines[:max(1, m.height)]
	}
	for n, line := range lines {
		lines[n] = ansi.Truncate(line, width, "")
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
