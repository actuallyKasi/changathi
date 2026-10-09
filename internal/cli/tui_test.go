package cli

import (
	"changathi/internal/core"
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func tuiFixture() *tuiModel {
	m := newTUI(context.Background(), nil, "a")
	m.plain = true
	m.connection = "Connected"
	m.renderer = nil
	m.snapshot = core.Snapshot{Projects: []core.Project{{ID: "p", Name: "demo"}}, Agents: []core.Attempt{{ID: "a", ProjectID: "p", Issue: core.Issue{Number: 7}, State: "running"}}}
	m.updateRows()
	m.resize()
	return m
}
func key(s string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: []rune(s)[0], Text: s} }

func TestStatusBadges(t *testing.T) {
	m := tuiFixture()
	for state, want := range map[string]string{
		"completed": "[✓ Completed]", "done": "[✓ Work finished]",
		"pending": "[◷ Not started]", "superseded": "[↪ Replaced]", "open": "[○ Issue open]",
	} {
		if got := m.statusBadge(state); got != want {
			t.Errorf("%s: got %q, want %q", state, got, want)
		}
	}
	if got := m.rowBadges(row{state: "pending", issueState: "open"}); got != "[◷ Not started] [○ Issue open]" {
		t.Fatal(got)
	}
	for _, dark := range []bool{true, false} {
		m.plain, m.dark = false, dark
		if got := ansi.Strip(m.rowBadges(row{state: "pending", issueState: "open"})); got != "[◷ Not started] [○ Issue open]" {
			t.Fatal(got)
		}
	}
}

func TestTranscriptScrollAndResizeAnchor(t *testing.T) {
	m := tuiFixture()
	s := m.state("a")
	s.loaded = true
	events := []core.Event{}
	for n := 1; n <= 40; n++ {
		events = append(events, core.Event{Seq: int64(n), TurnID: "t", Created: time.Unix(int64(n), 0), Output: &core.Output{Kind: "assistant", ItemID: strings.Repeat("x", n), Mode: "snapshot", Text: strings.Repeat("Long message with some words. ", 10), Status: "completed"}})
	}
	m.Update(pageMsg{attempt: "a", page: core.EventPage{Events: events, Cursor: 40}})
	if !m.viewport.AtBottom() {
		t.Fatal("initial output did not follow")
	}
	s.follow = false
	m.viewport.SetYOffset(m.itemOffsets[10])
	m.saveScroll()
	anchor := m.itemKeys[10]
	more := core.Event{Seq: 41, TurnID: "t", Output: &core.Output{Kind: "assistant", ItemID: "latest", Mode: "delta", Text: "new output"}, Created: time.Unix(41, 0)}
	m.Update(pageMsg{attempt: "a", page: core.EventPage{Events: []core.Event{more}, Cursor: 41}})
	if m.viewport.YOffset() != s.offset || s.unread != 1 || m.viewport.AtBottom() {
		t.Fatal("new output stole scroll position")
	}
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 22})
	idx := -1
	for n, k := range m.itemKeys {
		if k == anchor {
			idx = n
		}
	}
	if idx < 0 || m.viewport.YOffset() != m.itemOffsets[idx] {
		t.Fatal("resize lost logical anchor")
	}
}
func TestComposerShortcutIsolationAndDraft(t *testing.T) {
	m := tuiFixture()
	m.Update(key("m"))
	for _, r := range "qjm/r" {
		m.Update(key(string(r)))
	}
	if m.composer.Value() != "qjm/r" || m.raw || m.mode != "compose" {
		t.Fatal("input triggered shortcuts", m.composer.Value())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.state("a").draft != "qjm/r" {
		t.Fatal("draft lost")
	}
	m.Update(key("m"))
	if m.composer.Value() != "qjm/r" {
		t.Fatal("draft not restored")
	}
}
func TestHistoryReplayDedupAndFailureExpansion(t *testing.T) {
	m := tuiFixture()
	e := core.Event{Seq: 1, TurnID: "t", Output: &core.Output{Kind: "tool", ItemID: "call", Title: "go test", Text: "FAILED DETAIL", Mode: "snapshot", Status: "failed"}}
	p := pageMsg{attempt: "a", page: core.EventPage{Events: []core.Event{e}, Cursor: 1}}
	m.Update(p)
	m.Update(p)
	if len(m.state("a").events) != 1 || len(m.state("a").transcript.Items) != 1 {
		t.Fatal("duplicate replay")
	}
	if !strings.Contains(ansi.Strip(m.viewport.View()), "FAILED DETAIL") {
		t.Fatal("failed tool hidden")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if strings.Contains(ansi.Strip(m.viewport.View()), "FAILED DETAIL") {
		t.Fatal("cannot collapse failed tool")
	}
}
func TestStatusReorderKeepsSelectionAndSmallTerminal(t *testing.T) {
	m := tuiFixture()
	m.focused = ""
	m.snapshot.Agents = append(m.snapshot.Agents, core.Attempt{ID: "b", ProjectID: "p", State: "running", Created: time.Now()})
	m.Update(statusMsg{snapshot: m.snapshot})
	if m.selected != "a" {
		t.Fatal("refresh changed selection")
	}
	m.Update(tea.WindowSizeMsg{Width: 25, Height: 8})
	lines := strings.Split(m.View().Content, "\n")
	if len(lines) > 8 {
		t.Fatal("view exceeds height")
	}
	for _, line := range lines {
		if ansi.StringWidth(line) > 25 {
			t.Fatal("view exceeds width")
		}
	}
}

func TestComposerFitsAndAsyncCompletionPreservesNewDraft(t *testing.T) {
	m := tuiFixture()
	m.focused = ""
	m.connection = "Connected"
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m.Update(key("m"))
	m.composer.SetValue("new draft")
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "Ctrl-S send") || !strings.Contains(view, "Tab focus") || !strings.Contains(view, "new draft") {
		t.Fatal("composer/footer clipped", view)
	}
	m.Update(actionMsg{attempt: "a", text: "previous draft", message: core.Message{ID: "m", AttemptID: "a", Text: "previous draft", State: "queued"}})
	if m.composer.Value() != "new draft" || m.mode != "compose" {
		t.Fatal("async completion erased new draft")
	}
}

func TestReplacementAndStopConfirmation(t *testing.T) {
	m := tuiFixture()
	s := m.state("a")
	s.feedID = "old"
	s.loaded = true
	s.cursor = 100
	m.Update(pageMsg{attempt: "a", page: core.EventPage{FeedID: "replacement", Cursor: 100}})
	if s.loaded || s.cursor != 0 || !s.gap {
		t.Fatal("replacement did not reset cursor")
	}
	m.Update(key("s"))
	if m.mode != "confirm" || m.confirm.Attempt != "a" {
		t.Fatal("stop target missing")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.confirm != nil {
		t.Fatal("cancelled action retained")
	}
}

func TestPendingChildShowsItsOwnDetails(t *testing.T) {
	m := tuiFixture()
	m.focused = ""
	m.snapshot.Supervisors = []core.Supervisor{{ID: "plan", ProjectID: "p", Parent: core.Issue{Number: 10, Title: "Parent rollout overview"}, Plan: core.SupervisorPlan{Children: []core.SupervisorChild{{Issue: 6, DependsOn: []int{5}}}}, Children: []core.ChildStatus{{Issue: 5, Title: "Bootstrap Kubernetes", State: "awaiting_review"}, {Issue: 6, Title: "Validate the cluster", State: "pending", Reason: "Waiting for prerequisite review"}}}}
	m.updateRows()
	m.selected = "child:plan6"
	m.refresh(false)
	view := ansi.Strip(m.viewport.View())
	if strings.Contains(view, "Parent rollout overview") || !strings.Contains(view, "Validate the cluster") || !strings.Contains(view, "Bootstrap Kubernetes") || !strings.Contains(view, "Ready for review") {
		t.Fatal(view)
	}
}

func TestAgentActivityReflectsExecution(t *testing.T) {
	m := tuiFixture()
	m.connection = "Connected"
	for _, state := range []string{"preparing", "running", "validating", "pr_open", "merging", "queued", "retry_wait", "waiting_checks", "waiting_account", "awaiting_review", "done", "superseded", "cancelled", "needs_attention"} {
		m.snapshot.Agents[0].State = state
		m.updateRows()
		got := m.agentBadge(*m.selectedRow())
		active := state == "preparing" || state == "running" || state == "validating" || state == "pr_open" || state == "merging"
		if strings.Contains(got, "Agent active") != active || (!active && !strings.Contains(got, "Agent inactive")) {
			t.Fatalf("%s: %s", state, got)
		}
	}
	m.connection = "Disconnected"
	if got := m.agentBadge(*m.selectedRow()); !strings.Contains(got, "unknown") {
		t.Fatal(got)
	}
	if got := m.agentBadge(row{issue: 6}); !strings.Contains(got, "No agent yet") {
		t.Fatal(got)
	}
	if got := m.agentBadge(row{plan: &core.Supervisor{}}); got != "" {
		t.Fatal("supervisor is not a worker", got)
	}
}

func TestMouseWheelScrollAndFollow(t *testing.T) {
	m := tuiFixture()
	m.viewport.SetContent(strings.Repeat("output\n", 100))
	m.viewport.GotoBottom()
	m.Update(tea.MouseWheelMsg{X: 70, Y: 6, Button: tea.MouseWheelUp})
	if m.viewport.AtBottom() || m.state("a").follow {
		t.Fatal("wheel up did not pause following")
	}
	offset := m.viewport.YOffset()
	m.Update(tea.MouseWheelMsg{X: 70, Y: 1, Button: tea.MouseWheelUp})
	if m.viewport.YOffset() != offset {
		t.Fatal("header wheel scrolled output")
	}
	m.Update(tea.MouseWheelMsg{X: 70, Y: 6, Button: tea.MouseWheelDown})
	if !m.viewport.AtBottom() || !m.state("a").follow {
		t.Fatal("wheel down did not resume following at bottom")
	}
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("mouse reporting disabled")
	}
	m.focused = ""
	m.rows = []row{{id: "a", attempt: "a"}, {id: "b", attempt: "b"}}
	m.Update(tea.MouseWheelMsg{X: 2, Y: 6, Button: tea.MouseWheelDown})
	if m.selected != "b" || !m.sidebar {
		t.Fatal("wheel over list did not select next row")
	}
}

func TestConnectionErrorShownOnce(t *testing.T) {
	m := tuiFixture()
	m.focused = ""
	m.rows = nil
	m.connection = "Daemon unavailable · retrying · run: changathi daemon start"
	view := ansi.Strip(m.View().Content)
	if strings.Count(view, "Daemon unavailable") != 1 {
		t.Fatal(view)
	}
}

func TestInactiveAgentBlocksComposerAndSend(t *testing.T) {
	for _, state := range []string{"done", "awaiting_review", "superseded", "cancelled", "queued", "retry_wait", "waiting_checks", "needs_attention", "validating", "merging"} {
		m := tuiFixture()
		m.snapshot.Agents[0].State = state
		_, cmd := m.Update(key("m"))
		if m.mode == "compose" || cmd != nil || m.notice == "" {
			t.Fatalf("%s allowed composer", state)
		}
	}
	m := tuiFixture()
	m.Update(key("m"))
	m.composer.SetValue("keep this draft")
	m.snapshot.Agents[0].State = "awaiting_review"
	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if cmd != nil || m.sending || m.state("a").pending != nil {
		t.Fatal("inactive agent triggered send")
	}
	if m.state("a").draft != "keep this draft" || m.composer.Value() != "keep this draft" {
		t.Fatal("draft lost")
	}
	m.connection = "Disconnected"
	if m.messageBlockedReason() == "" {
		t.Fatal("disconnected send permitted")
	}
}

func TestSupervisorOwnsChildNavigation(t *testing.T) {
	m := tuiFixture()
	m.focused = ""
	m.snapshot.Supervisors = []core.Supervisor{{ID: "plan", Parent: core.Issue{Number: 10, Title: "Build the cluster"}, Children: []core.ChildStatus{{Issue: 3, Title: "First", State: "completed", AttemptID: "a"}, {Issue: 4, Title: "Second", State: "pending"}}}}
	m.snapshot.Agents = append(m.snapshot.Agents, core.Attempt{ID: "old", SupervisorID: "plan", State: "superseded"})
	m.updateRows()
	m.resize()
	if len(m.rows) != 2 || m.rows[0].issue != 3 {
		t.Fatalf("expected only children: %+v", m.rows)
	}
	m.selectRow(1)
	if m.selectedRow().issue != 4 {
		t.Fatal("navigation did not reach next child")
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "SUPERVISOR #10") || !strings.Contains(view, "Build the cluster") {
		t.Fatal(view)
	}
	if m.bodyTop() != 3 {
		t.Fatal("mouse layout not adjusted")
	}
	m.Update(key("h"))
	if len(m.rows) != 3 {
		t.Fatal("previous attempts inaccessible")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if m.group != "standalone" || m.currentSupervisor() != nil {
		t.Fatal("group switch failed")
	}
}
