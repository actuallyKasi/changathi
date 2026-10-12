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
	cancel context.CancelFunc
}
type Engine struct {
	supervisorMu sync.Mutex
	Config       core.Config
	Store        *core.Store
	GitHub       core.GitHub
	Providers    map[string]provider.Provider
	Workspaces   core.Workspaces
	usageMu      sync.Mutex
	mu           sync.Mutex
	workers      map[string]*worker
	locks        map[string]*sync.Mutex
	cursor       int
	login        string // operator login shown in status comments
	wake         chan struct{}
	wg           sync.WaitGroup
}

func New(c core.Config, s *core.Store, gh core.GitHub, providers map[string]provider.Provider) *Engine {
	return &Engine{Config: c, Store: s, GitHub: gh, Providers: providers, Workspaces: core.Workspaces{Root: c.DataDir}, workers: map[string]*worker{}, locks: map[string]*sync.Mutex{}, wake: make(chan struct{}, 1)}
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
		case "preparing", "running", "validating", "verifying", "pr_open", "merging": // preserve workspace and reconcile PR before any replacement execution
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
			flow, flowErr := e.Workspaces.LoadWorkflow(ctx, p)
			lock.Unlock()
			if flowErr == nil {
				flowErr = e.checkWorkflow(flow)
			}
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
		if p.Enabled {
			e.pollRework(ctx, p)
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
	e.publishStatus(ctx)
}
func (e *Engine) dispatch(ctx context.Context) {
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
		if loadErr == nil {
			loadErr = e.checkWorkflow(flow)
		}
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
			// Account gates apply per provider; delivery-only waits need no agent session.
			if a.State != "waiting_checks" && e.gate(a.Workflow.Implementer()) != nil {
				continue
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
	if accountLimit(err) {
		until := limitUntil(err, core.Now())
		role := core.Role{Provider: "codex"}
		var re roleError
		if errors.As(err, &re) {
			role = re.role
		}
		e.setGate(role, msg, until)
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
	// Rework stores the operator's comments just after enqueueing, so read them fresh.
	if cur, err := e.current(a.ID); err == nil {
		a.Feedback = cur.Feedback
	}
	issuePrompt, err := implementPrompt(p, a, flow)
	if err != nil {
		e.fail(ctx, a.ID, err)
		return
	}
	impl := flow.Implementer()
	prov, err := e.provider(impl)
	if err != nil {
		e.state(a.ID, "needs_attention", err.Error())
		return
	}
	// A thread belongs to the provider that created it; legacy records are Codex threads.
	thread, owner := a.Thread, a.ThreadProvider
	if owner == "" {
		owner = "codex"
	}
	if owner != impl.Provider {
		thread = ""
	}
	// The implement session opens only when a turn actually runs, so resuming at
	// validation or verification never starts (or bills) the implementer.
	var s provider.Session
	defer func() {
		if s != nil {
			_ = s.Close()
		}
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.PID = 0; return nil })
	}()
	open := func(thread string) error {
		if s != nil {
			_ = s.Close()
			s = nil
		}
		session, err := prov.Open(ctx)
		if err != nil {
			return err
		}
		s = session
		if proc, ok := s.(interface{ ProcessID() int }); ok {
			_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.PID = proc.ProcessID(); return nil })
		}
		if err = s.Start(ctx, provider.Request{Workspace: path, Thread: thread, Model: impl.Model, Effort: impl.Effort}); err != nil {
			return err
		}
		e.event(a.ID, "role", strings.TrimSpace(fmt.Sprintf("implement %s %s", impl.Provider, impl.Model)))
		return nil
	}
	setStep := func(step, failure string) {
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error {
			x.NextStep = step
			if failure != "" {
				x.LastFailure = failure
			}
			return nil
		})
	}
	prompt := issuePrompt
	resume := a.NextStep == "validate"
	if resume {
		e.event(a.ID, "resume", "implement turn already completed; resuming at validation")
	}
	threadID := thread
	for turns := a.Turns; turns < flow.MaxTurns || resume; turns++ {
		if err = e.eligible(ctx, p, a); err != nil {
			e.fail(ctx, a.ID, err)
			return
		}
		if resume {
			resume = false
			turns-- // no agent turn consumed
		} else {
			if s == nil {
				if g := e.gate(impl); g != nil {
					e.state(a.ID, "waiting_account", g.Reason)
					_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Due = g.Until; return nil })
					return
				}
				if err = open(thread); err != nil {
					e.fail(ctx, a.ID, roleError{impl, err})
					return
				}
			}
			e.state(a.ID, "running", "")
			_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Turns = turns + 1; x.NextStep = ""; x.Reply = ""; return nil })
			before := worktreeState(ctx, path)
			var final string
			var said []string
			run := func(prompt string) error {
				final, said = "", nil
				return s.Run(ctx, prompt, func(v provider.Event) {
					if v.Type == "claude/result" {
						final = v.Text
					} else if v.Output != nil && v.Output.Kind == "assistant" && v.Output.Mode == "snapshot" && v.Output.Text != "" {
						said = append(said, v.Output.Text)
					}
					if v.Thread != "" {
						threadID = v.Thread
						_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error {
							x.Thread, x.ThreadProvider, x.Turn = v.Thread, impl.Provider, v.Turn
							if v.PID != 0 {
								x.PID = v.PID
							}
							return nil
						})
					}
					e.recordEvent(core.Event{Version: 1, ProjectID: p.ID, AttemptID: a.ID, ThreadID: threadID, TurnID: v.Turn, Type: v.Type, Text: v.Text, Output: v.Output})
				})
			}
			err = run(prompt)
			if errors.Is(err, provider.ErrSessionLost) {
				// Start over in a fresh session, telling it what the branch already holds.
				e.event(a.ID, "session_lost", err.Error()+"; continuing in a new session")
				thread, threadID = "", ""
				_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Thread = ""; return nil })
				if err = open(""); err == nil {
					err = run(e.recoveryPrompt(ctx, p, path, issuePrompt, prompt))
				}
			}
			if err != nil {
				if accountLimit(err) {
					// A limited turn did no work; give it back so waiting out the limit cannot exhaust max_turns.
					_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Turns = turns; return nil })
				}
				e.fail(ctx, a.ID, roleError{impl, err})
				return
			}
			// A rework turn may answer the operator instead of changing code.
			if a.Feedback != "" {
				reply, ok := parseReply(final)
				if !ok {
					reply, ok = parseReply(strings.Join(said, "\n"))
				}
				if ok {
					if after := worktreeState(ctx, path); after != "" && after == before {
						e.answer(ctx, p, a, reply)
						return
					}
					e.event(a.ID, "reply", "ignoring the changathi-reply block because the turn changed the branch or worktree; validating the change instead")
				}
			}
			// The turn is complete: an interruption from here on resumes at validation.
			setStep("validate", "")
		}
		e.state(a.ID, "validating", "")
		head, base, validationErr := e.validate(ctx, p, a)
		if validationErr != nil {
			prompt = "Validation failed. Fix the problem and commit the correction:\n" + validationErr.Error()
			if errors.Is(validationErr, errNoCommits) && a.Feedback != "" {
				prompt += "\n\nIf the operator's comments need no code change, do not make one to satisfy this check. Leave the branch as it is and end your reply with a changathi-reply block holding your answer, as described earlier."
			}
			setStep("", prompt)
			continue
		}
		if verifier := flow.Verifier(); verifier != nil {
			latest, _ := e.current(a.ID)
			if v := latestVerification(latest, head); v != nil && v.Verdict == "pass" {
				e.event(a.ID, "verification", "pass already recorded for "+shortSHA(head)+"; not re-verifying")
			} else {
				if g := e.gate(*verifier); g != nil {
					e.state(a.ID, "waiting_account", g.Reason)
					_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Due = g.Until; return nil })
					return
				}
				e.state(a.ID, "verifying", "")
				rec, verifyErr := e.verify(ctx, p, a, *verifier, head, base)
				if errors.Is(verifyErr, errWorktreeChanged) {
					e.state(a.ID, "needs_attention", verifyErr.Error())
					return
				}
				if verifyErr != nil {
					e.fail(ctx, a.ID, verifyErr)
					return
				}
				switch rec.Verdict {
				case "malformed":
					e.state(a.ID, "needs_attention", "verifier returned no valid verdict twice: "+rec.Error)
					return
				case "fail":
					latest, _ := e.current(a.ID)
					blocking := core.FindingsText(rec.Findings, "blocking")
					prompt = "An independent reviewer found blocking problems in your change. Fix them and commit the corrections:\n" + blocking
					setStep("", prompt)
					if latest.VerifyFailures >= flow.VerifyRounds() {
						e.state(a.ID, "needs_attention", fmt.Sprintf("verification failed %d times; latest blocking findings:\n%s", latest.VerifyFailures, blocking))
						return
					}
					continue
				}
			}
		}
		if latest, err := e.current(a.ID); err == nil {
			a.Verifications = latest.Verifications
		}
		a.Head = head
		a.BaseSHA = base
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Head = head; x.BaseSHA = base; x.LastFailure = ""; return nil })
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
		e.postVerificationNotes(ctx, p, a, pr)
		e.deliver(ctx, p, a, pr)
		return
	}
	e.fail(ctx, a.ID, errors.New("validation or verification failed after maximum agent turns"))
}

// implementPrompt is the full first-turn prompt: the issue, delivery rules, the
// previous generation's outcome for rework, the operator's review comments, and
// the last failure to address.
func implementPrompt(p core.Project, a core.Attempt, flow core.Workflow) (string, error) {
	body, err := flow.Render(a.Issue)
	if err != nil {
		return "", err
	}
	prompt := fmt.Sprintf("Work on GitHub issue #%d in %s.\nTitle: %s\nDescription:\n%s\n\n%s\n\nImplement changes and commit them on the existing branch. Do not push, create/merge PRs, close issues, or use closing keywords in commits. Changathi handles delivery after validating your changes. Leave the issue open for human review. Run the configured validation commands: %v.", a.Issue.Number, p.Repo, a.Issue.Title, a.Issue.Body, body, flow.Validation)
	if prev := a.Previous; prev != nil {
		prompt += fmt.Sprintf("\n\nThis is rework: generation %d of this issue came before you.", prev.Generation)
		switch {
		case prev.PR != 0 && (prev.State == "awaiting_review" || prev.State == "done" || prev.State == "superseded"):
			prompt += fmt.Sprintf(" Its PR #%d was merged, so that work is already on your branch. Change only what the issue still needs.", prev.PR)
		case prev.PR != 0:
			prompt += fmt.Sprintf(" Its PR #%d was not merged (it ended %s).", prev.PR, prev.State)
		default:
			prompt += fmt.Sprintf(" It ended %s without a PR.", prev.State)
		}
		if prev.LastFailure != "" {
			prompt += "\nIts last failure:\n" + prev.LastFailure
		}
	}
	if a.Feedback != "" {
		prompt += "\n\n" + a.Feedback
		prompt += "\n\n" + replyInstructions
	}
	if a.LastFailure != "" {
		prompt += "\n\nThe last failure on this branch, still to be fixed:\n" + a.LastFailure
	}
	return prompt, nil
}

// recoveryPrompt rebuilds context for a fresh session after a lost one: the
// issue, what the branch already holds, and the turn's own instruction.
func (e *Engine) recoveryPrompt(ctx context.Context, p core.Project, path, issuePrompt, turnPrompt string) string {
	var b strings.Builder
	b.WriteString("Your earlier session for this issue could not be resumed, so you are starting without its history. Continue the work already on this branch rather than starting over.\n")
	for _, part := range []struct{ title, cmd string }{
		{"Commits on the branch", "log --oneline origin/" + p.Base + "..HEAD"},
		{"Changed files", "diff --stat origin/" + p.Base + "...HEAD"},
		{"Uncommitted changes", "status --porcelain"},
	} {
		out, err := core.Command(ctx, path, "git", strings.Fields(part.cmd)...)
		if err != nil || strings.TrimSpace(out) == "" {
			out = "(none)"
		}
		fmt.Fprintf(&b, "\n%s:\n%s\n", part.title, out)
	}
	b.WriteString("\n")
	b.WriteString(issuePrompt)
	if turnPrompt != issuePrompt {
		b.WriteString("\n\nCurrent task:\n")
		b.WriteString(turnPrompt)
	}
	return b.String()
}
func (e *Engine) event(id, typ, text string) {
	e.recordEvent(core.Event{AttemptID: id, Type: typ, Text: text})
}
func (e *Engine) recordEvent(event core.Event) {
	id, typ, text := event.AttemptID, event.Type, event.Text
	activity := typ
	if event.Output != nil && event.Output.Kind == "assistant" && event.Output.Mode == "snapshot" && text != "" {
		activity = strings.ReplaceAll(text, "\n", " ")
		if len(activity) > 160 {
			activity = activity[:160]
		}
	}
	if typ == "item/agentMessage/delta" && text != "" {
		activity = strings.ReplaceAll(text, "\n", " ")
		if len(activity) > 160 {
			activity = activity[:160]
		}
	}
	_ = e.Store.UpdateAttempt(id, func(a *core.Attempt) error { a.Activity = activity; return nil })
	if typ == "claude/usage" || typ == "claude/rate_limit" {
		e.recordUsage(typ, text)
	}
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

var errNoCommits = errors.New("agent made no commits")

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
		return "", "", errNoCommits
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
			_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.LastFailure = err.Error(); x.NextStep = ""; return nil })
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
