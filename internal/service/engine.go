package service

import (
	"changathi/internal/core"
	"changathi/internal/provider"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type worker struct {
	cancel     context.CancelFunc
	session    provider.Session
	activeTurn bool
}
type Engine struct {
	supervisorMu sync.Mutex
	Config       core.Config
	Store        *core.Store
	GitHub       core.GitHub
	Provider     provider.Provider
	Workspaces   core.Workspaces
	mu           sync.Mutex
	workers      map[string]*worker
	locks        map[string]*sync.Mutex
	cursor       int
	wake         chan struct{}
	wg           sync.WaitGroup
}

func New(c core.Config, s *core.Store, gh core.GitHub, p provider.Provider) *Engine {
	return &Engine{Config: c, Store: s, GitHub: gh, Provider: p, Workspaces: core.Workspaces{Root: c.DataDir}, workers: map[string]*worker{}, locks: map[string]*sync.Mutex{}, wake: make(chan struct{}, 1)}
}
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) lock(id string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		e.locks[id] = m
	}
	return m
}
func (e *Engine) state(id, state, reason string) {
	_ = e.Store.UpdateAttempt(id, func(a *core.Attempt) error {
		if a.State == "cancelled" {
			return nil
		}
		a.State = state
		a.Error = reason
		return nil
	})
	text := state
	if reason != "" {
		text += ": " + reason
	}
	_ = e.Store.Event(id, "state", text)
}
func (e *Engine) current(id string) (core.Attempt, error) {
	var a core.Attempt
	err := e.Store.Get("attempt", id, &a)
	return a, err
}
func (e *Engine) eligible(ctx context.Context, p core.Project, a core.Attempt) error {
	latest, err := e.Store.Project(p.ID)
	if err != nil {
		return err
	}
	if !latest.Enabled {
		return fmt.Errorf("project is paused")
	}
	cur, err := e.current(a.ID)
	if err != nil {
		return err
	}
	if cur.State == "cancelled" {
		return context.Canceled
	}
	i, err := e.GitHub.Issue(ctx, p, a.Issue.Number)
	if err != nil {
		return err
	}
	if i.State != "open" {
		return fmt.Errorf("issue is closed")
	}
	if i.Supervisor() {
		return fmt.Errorf("parent issues cannot run as implementation workers")
	}
	if a.Trigger == "auto" && !i.Ready() {
		return fmt.Errorf("automatic issue no longer has agent:ready")
	}
	return e.supervisorGate(ctx, p, a)
}
func (e *Engine) Run(ctx context.Context) error {
	as, err := core.List[core.Attempt](e.Store, "attempt")
	if err != nil {
		return err
	}
	for _, a := range as {
		switch a.State {
		case "preparing", "running", "validating", "pr_open", "merging": // preserve workspace and reconcile PR before any replacement execution
			guard, lockErr := core.ExecutionLock(e.Config.DataDir, a.ID)
			if lockErr != nil {
				e.state(a.ID, "needs_attention", lockErr.Error())
				continue
			}
			guard.Close()
			_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.PID = 0; return nil })
			e.state(a.ID, "retry_wait", "recovering interrupted operation")
		}
	}
	messages, _ := core.List[core.Message](e.Store, "message")
	for _, m := range messages {
		if m.State == "sent" {
			m.State = "uncertain"
			m.Error = "daemon interrupted delivery"
			_ = e.Store.Put("message", m.ID, m)
		}
	}
	ticker := time.NewTicker(time.Duration(e.Config.PollSeconds) * time.Second)
	defer ticker.Stop()
	e.poll(ctx)
	e.dispatch(ctx)
	for {
		select {
		case <-ctx.Done():
			e.mu.Lock()
			for _, w := range e.workers {
				w.cancel()
			}
			e.mu.Unlock()
			e.wg.Wait()
			return nil
		case <-ticker.C:
			e.poll(ctx)
			e.dispatch(ctx)
		case <-e.wake:
			e.dispatch(ctx)
		}
	}
}
func (e *Engine) poll(ctx context.Context) {
	e.pollSupervisors(ctx)
	ps, err := core.List[core.Project](e.Store, "project")
	if err != nil {
		return
	}
	for _, p := range ps {
		if ctx.Err() != nil {
			return
		}
		if p.Enabled && p.Error != "" {
			lock := e.lock(p.ID)
			lock.Lock()
			_, flowErr := e.Workspaces.LoadWorkflow(ctx, p)
			lock.Unlock()
			if flowErr == nil {
				_ = e.Store.UpdateProject(p.ID, func(x *core.Project) error { x.Error = ""; return nil })
				as, _ := core.List[core.Attempt](e.Store, "attempt")
				for _, a := range as {
					if a.ProjectID == p.ID && a.State == "needs_attention" && strings.HasPrefix(a.Error, "workflow:") {
						e.state(a.ID, "queued", "workflow corrected")
					}
				}
			}
		}
		if p.Enabled && p.Auto {
			xs, err := e.GitHub.Issues(ctx, p, true)
			if err != nil {
				_ = e.Store.Event("", "github_error", p.Name+": "+err.Error())
				continue
			}
			for _, i := range xs {
				_, _ = e.Store.Enqueue(p, i, "auto", false)
			}
		}
	}
	as, _ := core.List[core.Attempt](e.Store, "attempt")
	for _, a := range as {
		if a.Active() || (a.State == "cancelled" && a.Trigger == "auto") {
			p, err := e.Store.Project(a.ProjectID)
			if err != nil {
				continue
			}
			i, err := e.GitHub.Issue(ctx, p, a.Issue.Number)
			if err != nil {
				continue
			}
			if a.Active() && (i.State != "open" || (a.Trigger == "auto" && !i.Ready())) {
				_ = e.Cancel(a.ID)
			}
			if a.Trigger == "auto" && !i.Ready() {
				_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.ReadyRemoved = true; return nil })
			}
			if a.State == "cancelled" && a.ReadyRemoved && i.Ready() && i.State == "open" && p.Enabled && p.Auto {
				_, _ = e.Store.Enqueue(p, i, "auto", true)
			}
		}
		if a.State == "awaiting_review" && a.SupervisorID == "" {
			p, _ := e.Store.Project(a.ProjectID)
			i, err := e.GitHub.Issue(ctx, p, a.Issue.Number)
			if err == nil && i.State == "closed" {
				e.state(a.ID, "done", "")
			}
		}
	}
}
func (e *Engine) dispatch(ctx context.Context) {
	var gate core.ProviderGate
	if e.Store.Get("gate", "codex", &gate) == nil && gate.Until.After(core.Now()) {
		return
	}
	ps, err := core.List[core.Project](e.Store, "project")
	if err != nil || len(ps) == 0 {
		return
	}
	as, err := core.List[core.Attempt](e.Store, "attempt")
	if err != nil {
		return
	}
	profiles := map[string]core.Workflow{}
	for _, p := range ps {
		if !p.Enabled || p.Error != "" {
			continue
		}
		needed := false
		for _, a := range as {
			if a.ProjectID == p.ID && a.Runnable() && a.Workflow.Revision == "" {
				needed = true
				break
			}
		}
		if !needed {
			continue
		}
		lock := e.lock(p.ID)
		lock.Lock()
		flow, loadErr := e.Workspaces.LoadWorkflow(ctx, p)
		lock.Unlock()
		if loadErr != nil {
			_ = e.Store.UpdateProject(p.ID, func(x *core.Project) error { x.Error = loadErr.Error(); return nil })
			_ = e.Store.Event("", "workflow_error", p.Name+": "+loadErr.Error())
			continue
		}
		profiles[p.ID] = flow
	}
	for step := 0; step < len(ps)*e.Config.Concurrency; step++ {
		p := ps[(e.cursor+step)%len(ps)]
		latest, projectErr := e.Store.Project(p.ID)
		if projectErr != nil {
			continue
		}
		p = latest
		if !p.Enabled || p.Error != "" {
			continue
		}
		for _, a := range as {
			if a.ProjectID != p.ID || !a.Runnable() {
				continue
			}
			if a.Workflow.Revision == "" {
				flow, ok := profiles[p.ID]
				if !ok {
					continue
				}
				a.Workflow = flow
				if err := e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Workflow = flow; return nil }); err != nil {
					continue
				}
			}
			if a.SupervisorID != "" {
				if err := e.eligible(ctx, p, a); err != nil {
					continue
				}
			}
			e.mu.Lock()
			count := 0
			for id := range e.workers {
				for _, active := range as {
					if active.ID == id && active.ProjectID == p.ID {
						count++
					}
				}
			}
			limit := p.Concurrency
			if limit < 1 {
				limit = 1
			}
			if a.Workflow.Concurrency > 0 && a.Workflow.Concurrency < limit {
				limit = a.Workflow.Concurrency
			}
			_, exists := e.workers[a.ID]
			if exists {
				e.mu.Unlock()
				continue
			}
			if len(e.workers) >= e.Config.Concurrency || count >= limit {
				e.mu.Unlock()
				break
			}
			jobCtx, cancel := context.WithCancel(ctx)
			e.workers[a.ID] = &worker{cancel: cancel}
			e.mu.Unlock()
			e.wg.Add(1)
			go func(p core.Project, a core.Attempt) {
				defer e.wg.Done()
				defer func() { e.mu.Lock(); delete(e.workers, a.ID); e.mu.Unlock(); e.Wake() }()
				e.execute(jobCtx, p, a)
			}(p, a)
			break
		}
	}
	e.cursor = (e.cursor + 1) % len(ps)
}
func (e *Engine) Cancel(id string) error {
	a, err := e.current(id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if w := e.workers[id]; w != nil {
		w.cancel()
	}
	e.mu.Unlock()
	lock := e.lock(a.ProjectID)
	lock.Lock()
	defer lock.Unlock()
	if err := e.Store.UpdateAttempt(id, func(a *core.Attempt) error {
		if a.State == "done" || a.State == "awaiting_review" || a.State == "superseded" {
			return errors.New("delivered work cannot be cancelled; use issue retry for rework")
		}
		a.State = "cancelled"
		a.Error = "cancelled by operator"
		return nil
	}); err != nil {
		return err
	}
	e.mu.Lock()
	if w := e.workers[id]; w != nil {
		w.cancel()
	}
	e.mu.Unlock()
	e.Wake()
	return nil
}
func (e *Engine) Message(ctx context.Context, id, text string) (core.Message, error) {
	a, err := e.current(id)
	if err != nil {
		return core.Message{}, err
	}
	if !a.Active() {
		return core.Message{}, errors.New("agent is not active")
	}
	if a.State == "validating" || a.State == "merging" || a.State == "pr_open" {
		return core.Message{}, errors.New("agent has finished its turn and delivery is in progress; use issue retry after delivery for rework")
	}
	if strings.TrimSpace(text) == "" {
		return core.Message{}, errors.New("message text is empty")
	}
	m := core.Message{ID: core.ID(), AttemptID: id, Text: text, State: "queued", Created: core.Now()}
	if err = e.Store.Put("message", m.ID, m); err != nil {
		return m, err
	}
	e.mu.Lock()
	w := e.workers[id]
	var s provider.Session
	if w != nil && w.activeTurn {
		s = w.session
	}
	e.mu.Unlock()
	if s != nil {
		m.State = "sent"
		if err = e.Store.Put("message", m.ID, m); err != nil {
			return m, err
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = s.Steer(callCtx, text, m.ID)
		cancel()
		if err != nil {
			m.State = "uncertain"
			m.Error = err.Error()
		} else {
			m.State = "acknowledged"
		}
		err = e.Store.Put("message", m.ID, m)
	} else if a.State == "waiting_checks" {
		e.state(id, "queued", "operator message queued")
		_ = e.Store.UpdateAttempt(id, func(x *core.Attempt) error { x.Due = time.Time{}; return nil })
		e.Wake()
	}
	return m, err
}
func (e *Engine) fail(ctx context.Context, id string, err error) {
	a, _ := e.current(id)
	if a.State == "cancelled" {
		return
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			e.state(id, "needs_attention", "attempt timeout exceeded; work preserved")
			return
		}
		e.state(id, "retry_wait", "interrupted; work preserved")
		return
	}
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "rate limit") || strings.Contains(strings.ToLower(msg), "usage limit") || strings.Contains(strings.ToLower(msg), "ratelimit") || strings.Contains(strings.ToLower(msg), "quota") {
		until := core.Now().Add(5 * time.Minute)
		_ = e.Store.Put("gate", "codex", core.ProviderGate{Until: until, Reason: msg})
		e.state(id, "waiting_account", msg)
		_ = e.Store.UpdateAttempt(id, func(a *core.Attempt) error { a.Due = until; return nil })
		return
	}
	low := strings.ToLower(msg)
	transient := false
	for _, fragment := range []string{"connection reset", "connection refused", "temporary failure", "timeout", "timed out", "http 502", "http 503", "http 504", "unexpected eof"} {
		if strings.Contains(low, fragment) {
			transient = true
		}
	}
	if transient && a.Retries < 3 {
		e.state(id, "retry_wait", msg)
		_ = e.Store.UpdateAttempt(id, func(x *core.Attempt) error {
			x.Retries++
			x.Due = core.Now().Add(time.Duration(1<<x.Retries)*5*time.Second + time.Duration(core.Now().UnixNano()%1000)*time.Millisecond)
			return nil
		})
		return
	}
	e.state(id, "needs_attention", msg)
}
func (e *Engine) execute(ctx context.Context, p core.Project, a core.Attempt) {
	guard, lockErr := core.ExecutionLock(e.Config.DataDir, a.ID)
	if lockErr != nil {
		e.fail(ctx, a.ID, lockErr)
		return
	}
	defer guard.Close()
	ctx = core.WithExecutionLock(ctx, guard)
	if a.PR != 0 {
		pr, err := e.GitHub.FindPR(ctx, p, a.Branch)
		if err != nil {
			e.fail(ctx, a.ID, err)
			return
		}
		if pr != nil && pr.State == "MERGED" {
			e.review(ctx, p, a, pr)
			return
		}
	}
	if err := e.eligible(ctx, p, a); err != nil {
		e.fail(ctx, a.ID, err)
		return
	}
	lock := e.lock(p.ID)
	lock.Lock()
	e.state(a.ID, "preparing", "")
	path, flow, err := e.Workspaces.Prepare(ctx, p, a)
	lock.Unlock()
	if err != nil {
		if strings.HasPrefix(err.Error(), "workflow:") {
			_ = e.Store.UpdateProject(p.ID, func(x *core.Project) error { x.Error = err.Error(); return nil })
		}
		e.fail(ctx, a.ID, err)
		return
	}
	if a.Workflow.Revision != "" {
		flow = a.Workflow
	}
	a.Workspace = path
	a.Workflow = flow
	_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Workspace = path; x.Workflow = flow; return nil })
	ctx, cancel := context.WithTimeout(ctx, time.Duration(flow.TimeoutSeconds)*time.Second)
	defer cancel()
	// PR reconciliation always precedes rerunning the agent or publication.
	pr, err := e.GitHub.FindPR(ctx, p, a.Branch)
	if err != nil {
		e.fail(ctx, a.ID, err)
		return
	}
	if pr != nil && pr.State == "MERGED" {
		e.review(ctx, p, a, pr)
		return
	}
	if pr != nil && pr.State == "CLOSED" {
		e.fail(ctx, a.ID, errors.New("managed PR was closed without merging; retry as a new generation"))
		return
	}
	if a.State == "waiting_checks" && pr != nil {
		e.deliver(ctx, p, a, pr)
		return
	}
	prompt, err := flow.Render(a.Issue)
	if err != nil {
		e.fail(ctx, a.ID, err)
		return
	}
	prompt = fmt.Sprintf("Work on GitHub issue #%d in %s.\nTitle: %s\nDescription:\n%s\n\n%s\n\nImplement changes and commit them on the existing branch. Do not push, create/merge PRs, close issues, or use closing keywords in commits. Changathi handles delivery after validating your changes. Leave the issue open for human review. Run the configured validation commands: %v.", a.Issue.Number, p.Repo, a.Issue.Title, a.Issue.Body, prompt, flow.Validation)
	if a.Error != "" {
		prompt += "\nPrevious attempt/check outcome: " + a.Error
	}
	s, err := e.Provider.Open(ctx)
	if err != nil {
		e.fail(ctx, a.ID, err)
		return
	}
	if proc, ok := s.(interface{ ProcessID() int }); ok {
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.PID = proc.ProcessID(); return nil })
	}
	defer func() {
		_ = s.Close()
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.PID = 0; return nil })
	}()
	if err = s.Start(ctx, provider.Request{Workspace: path, Thread: a.Thread, Model: flow.Model}); err != nil {
		e.fail(ctx, a.ID, err)
		return
	}
	e.mu.Lock()
	if w := e.workers[a.ID]; w != nil {
		w.session = s
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		if w := e.workers[a.ID]; w != nil {
			w.session = nil
		}
		e.mu.Unlock()
	}()
	threadID := a.Thread
	for turns := a.Turns; turns < flow.MaxTurns; turns++ {
		if err = e.eligible(ctx, p, a); err != nil {
			e.fail(ctx, a.ID, err)
			return
		}
		messages, _ := core.List[core.Message](e.Store, "message")
		for _, m := range messages {
			if m.AttemptID == a.ID && m.State == "queued" {
				prompt += "\nOperator message [" + m.ID + "]: " + m.Text
				m.State = "sent"
				_ = e.Store.Put("message", m.ID, m)
			}
		}
		e.state(a.ID, "running", "")
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Turns = turns + 1; return nil })
		err = s.Run(ctx, prompt, func(v provider.Event) {
			if v.Thread != "" {
				threadID = v.Thread
				e.mu.Lock()
				if w := e.workers[a.ID]; w != nil {
					w.activeTurn = true
				}
				e.mu.Unlock()
				_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Thread = v.Thread; x.Turn = v.Turn; x.PID = v.PID; return nil })
				for _, m := range messages {
					if m.AttemptID == a.ID && m.State == "queued" {
						m.State = "acknowledged"
						_ = e.Store.Put("message", m.ID, m)
					}
				}
			}
			e.recordEvent(core.Event{Version: 1, ProjectID: p.ID, AttemptID: a.ID, ThreadID: threadID, TurnID: v.Turn, Type: v.Type, Text: v.Text, Output: v.Output})
		})
		e.mu.Lock()
		if w := e.workers[a.ID]; w != nil {
			w.activeTurn = false
		}
		e.mu.Unlock()
		if err != nil {
			for _, m := range messages {
				if m.AttemptID == a.ID && m.State == "queued" {
					var latest core.Message
					if e.Store.Get("message", m.ID, &latest) == nil && latest.State == "sent" {
						latest.State = "uncertain"
						latest.Error = err.Error()
						_ = e.Store.Put("message", latest.ID, latest)
					}
				}
			}
			e.fail(ctx, a.ID, err)
			return
		}
		pending, _ := core.List[core.Message](e.Store, "message")
		hasQueued := false
		for _, m := range pending {
			if m.AttemptID == a.ID && m.State == "queued" {
				hasQueued = true
			}
		}
		if hasQueued {
			prompt = "Address the queued operator instructions and commit your changes."
			continue
		}
		e.state(a.ID, "validating", "")
		head, base, validationErr := e.validate(ctx, p, a)
		if validationErr != nil {
			prompt = "Validation failed. Fix the problem and commit the correction:\n" + validationErr.Error()
			continue
		}
		a.Head = head
		a.BaseSHA = base
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Head = head; x.BaseSHA = base; return nil })
		if err = e.eligible(ctx, p, a); err != nil {
			e.fail(ctx, a.ID, err)
			return
		}
		if _, err = core.Command(ctx, path, "git", "push", "origin", "HEAD:refs/heads/"+a.Branch); err != nil {
			e.fail(ctx, a.ID, err)
			return
		}
		pr, err = e.GitHub.FindPR(ctx, p, a.Branch)
		if err == nil && pr == nil {
			err = e.operation(a.ID, "create_pr", func() error { var callErr error; pr, callErr = e.GitHub.CreatePR(ctx, p, a); return callErr })
		}
		if err != nil {
			e.fail(ctx, a.ID, err)
			return
		}
		if pr == nil {
			e.fail(ctx, a.ID, errors.New("PR creation returned no PR"))
			return
		}
		e.deliver(ctx, p, a, pr)
		return
	}
	e.fail(ctx, a.ID, errors.New("validation failed after maximum agent turns"))
}
func (e *Engine) event(id, typ, text string) {
	e.recordEvent(core.Event{AttemptID: id, Type: typ, Text: text})
}
func (e *Engine) recordEvent(event core.Event) {
	id, typ, text := event.AttemptID, event.Type, event.Text
	activity := typ
	if typ == "item/agentMessage/delta" && text != "" {
		activity = strings.ReplaceAll(text, "\n", " ")
		if len(activity) > 160 {
			activity = activity[:160]
		}
	}
	_ = e.Store.UpdateAttempt(id, func(a *core.Attempt) error { a.Activity = activity; return nil })
	if typ == "account/rateLimits/updated" {
		_ = e.Store.Put("usage", "codex", map[string]any{"reported": text})
	}
	if len(text) > 65536 {
		text = strings.ToValidUTF8(text[:65536], "") + " [truncated]"
	}
	event.Text = text
	if event.Output != nil && len(event.Output.Text) > 65536 {
		event.Output.Text = strings.ToValidUTF8(event.Output.Text[:65536], "") + "\n[truncated]"
	}
	if event.Output != nil && len(event.Output.Title) > 4096 {
		event.Output.Title = strings.ToValidUTF8(event.Output.Title[:4096], "") + "…"
	}
	_ = e.Store.AppendEvent(event)
	dir := filepath.Join(e.Config.DataDir, "logs")
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	path := filepath.Join(dir, id+".log")
	if info, err := os.Stat(path); err == nil && info.Size()+int64(len(text)) > e.Config.MaxLogBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		defer f.Close()
		fmt.Fprintf(f, "%s %s %s\n", core.Now().Format(time.RFC3339), typ, text)
	}
}

var closing = regexp.MustCompile(`(?i)\b(close[sd]?|fix(e[sd])?|resolve[sd]?)\s+(#[0-9]+|[a-z0-9_.-]+/[a-z0-9_.-]+#[0-9]+|https://github\.com/\S+/issues/[0-9]+)`)

func (e *Engine) validate(ctx context.Context, p core.Project, a core.Attempt) (string, string, error) {
	base, err := e.Workspaces.RefreshBase(ctx, p)
	if err != nil {
		return "", "", err
	}
	if _, err = core.Command(ctx, a.Workspace, "git", "merge", "--no-edit", base); err != nil {
		return "", "", fmt.Errorf("base merge failed: %w", err)
	}
	for _, command := range a.Workflow.Validation {
		out, err := core.Command(ctx, a.Workspace, "sh", "-c", command)
		e.event(a.ID, "validation", out)
		if err != nil {
			return "", "", err
		}
	}
	dirty, err := core.Command(ctx, a.Workspace, "git", "status", "--porcelain")
	if err != nil {
		return "", "", err
	}
	if dirty != "" {
		return "", "", errors.New("uncommitted changes remain after validation")
	}
	commits, err := core.Command(ctx, a.Workspace, "git", "log", "--format=%B", base+"..HEAD")
	if err != nil {
		return "", "", err
	}
	if closing.MatchString(commits) {
		return "", "", errors.New("commit contains an issue-closing keyword; amend it")
	}
	count, err := core.Command(ctx, a.Workspace, "git", "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return "", "", err
	}
	if count == "0" {
		return "", "", errors.New("agent made no commits")
	}
	head, err := core.Command(ctx, a.Workspace, "git", "rev-parse", "HEAD")
	return head, base, err
}
func CheckPR(p core.Project, a core.Attempt, pr *core.PR) (string, error) {
	if pr.Branch != a.Branch || pr.Base != p.Base {
		return "", errors.New("PR branch/base mismatch")
	}
	if len(pr.Closing) > 0 || closing.MatchString(pr.Body) || closing.MatchString(pr.Title) {
		return "", errors.New("PR has issue-closing references")
	}
	if pr.Head != a.Head || pr.BaseSHA != a.BaseSHA {
		return "revalidate", nil
	}
	found := map[string]bool{}
	for _, c := range pr.Checks {
		found[c.Key()] = c.Success()
		if c.Pending() {
			return "wait", nil
		}
		if !c.Success() {
			return "", fmt.Errorf("check %s failed or is indeterminate", c.Key())
		}
	}
	for _, name := range a.Workflow.RequiredChecks {
		if !found[name] {
			return "wait", nil
		}
	}
	if pr.Mergeable == "CONFLICTING" {
		return "revalidate", nil
	}
	if pr.Mergeable != "MERGEABLE" || pr.MergeState == "UNKNOWN" {
		return "wait", nil
	}
	if pr.MergeState != "CLEAN" && pr.MergeState != "HAS_HOOKS" {
		return "", fmt.Errorf("repository rules prevent merge: %s", pr.MergeState)
	}
	return "merge", nil
}
func (e *Engine) deliver(ctx context.Context, p core.Project, a core.Attempt, pr *core.PR) {
	lock := e.lock(p.ID)
	lock.Lock()
	defer lock.Unlock()
	latest, err := e.GitHub.FindPR(ctx, p, a.Branch)
	if err != nil {
		e.fail(ctx, a.ID, err)
		return
	}
	if latest == nil {
		e.fail(ctx, a.ID, errors.New("managed PR disappeared"))
		return
	}
	pr = latest
	if pr.State == "MERGED" {
		e.review(ctx, p, a, pr)
		return
	}
	if pr.State != "OPEN" {
		e.fail(ctx, a.ID, errors.New("managed PR is not open"))
		return
	}
	_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.PR = pr.Number; return nil })
	a, _ = e.current(a.ID)
	decision, err := CheckPR(p, a, pr)
	if err != nil {
		if strings.Contains(err.Error(), "check ") && a.Turns < a.Workflow.MaxTurns {
			e.state(a.ID, "queued", err.Error())
			return
		}
		e.fail(ctx, a.ID, err)
		return
	}
	if decision == "revalidate" {
		head, base, err := e.validate(ctx, p, a)
		if err != nil {
			e.fail(ctx, a.ID, err)
			return
		}
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Head = head; x.BaseSHA = base; return nil })
		if err = e.eligible(ctx, p, a); err != nil {
			e.fail(ctx, a.ID, err)
			return
		}
		_, err = core.Command(ctx, a.Workspace, "git", "push", "origin", "HEAD:refs/heads/"+a.Branch)
		if err != nil {
			e.fail(ctx, a.ID, err)
			return
		}
		e.wait(a.ID, "waiting for checks on updated head/base")
		return
	}
	if decision == "wait" {
		e.wait(a.ID, "waiting for GitHub checks or mergeability")
		return
	}
	if err = e.eligible(ctx, p, a); err != nil {
		e.fail(ctx, a.ID, err)
		return
	}
	e.state(a.ID, "merging", "")
	err = e.operation(a.ID, "merge", func() error { return e.GitHub.Merge(ctx, p, pr, a.Workflow.MergeMethod) })
	if err != nil { // network failures may follow successful merges
		latest, check := e.GitHub.FindPR(ctx, p, a.Branch)
		if check != nil || latest == nil || latest.State != "MERGED" {
			e.fail(ctx, a.ID, err)
			return
		}
		pr = latest
	}
	confirmed, checkErr := e.GitHub.FindPR(ctx, p, a.Branch)
	if checkErr != nil {
		e.fail(ctx, a.ID, checkErr)
		return
	}
	if confirmed == nil || confirmed.State != "MERGED" {
		e.wait(a.ID, "merge accepted; awaiting GitHub confirmation")
		return
	}
	e.review(ctx, p, a, confirmed)
}
func (e *Engine) wait(id, reason string) {
	e.state(id, "waiting_checks", reason)
	_ = e.Store.UpdateAttempt(id, func(a *core.Attempt) error {
		a.Due = core.Now().Add(time.Duration(e.Config.PollSeconds) * time.Second)
		return nil
	})
}
func (e *Engine) review(ctx context.Context, p core.Project, a core.Attempt, pr *core.PR) {
	_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.PR = pr.Number; return nil })
	if err := e.GitHub.MarkReview(ctx, p, a.Issue.Number); err != nil {
		e.fail(ctx, a.ID, fmt.Errorf("PR merged, review labels need reconciliation: %w", err))
		return
	}
	i, err := e.GitHub.Issue(ctx, p, a.Issue.Number)
	if err != nil {
		e.fail(ctx, a.ID, err)
		return
	}
	if i.State != "open" {
		e.fail(ctx, a.ID, errors.New("PR merged but issue unexpectedly closed; review discrepancy"))
		return
	}
	e.state(a.ID, "awaiting_review", "")
}
func (e *Engine) operation(id, kind string, fn func() error) error {
	op := core.Operation{ID: core.ID(), AttemptID: id, Kind: kind, State: "intent"}
	if err := e.Store.Put("operation", op.ID, op); err != nil {
		return err
	}
	err := fn()
	op.State = "complete"
	if err != nil {
		op.State = "uncertain"
		op.Result = err.Error()
	}
	if save := e.Store.Put("operation", op.ID, op); save != nil && err == nil {
		return save
	}
	return err
}
