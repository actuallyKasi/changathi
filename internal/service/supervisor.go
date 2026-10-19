package service

import (
	"changathi/internal/core"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

func (e *Engine) supervisorAdapter() (core.SupervisorGitHub, error) {
	g, ok := e.GitHub.(core.SupervisorGitHub)
	if !ok {
		return nil, fmt.Errorf("GitHub adapter does not support supervision")
	}
	return g, nil
}
func (e *Engine) loadSupervisor(ctx context.Context, p core.Project, n int) (core.Supervisor, error) {
	parent, err := e.GitHub.Issue(ctx, p, n)
	if err != nil {
		return core.Supervisor{}, err
	}
	if parent.ID == 0 || parent.Number != n || parent.State != "open" || !parent.Supervisor() {
		return core.Supervisor{}, fmt.Errorf("parent must be an open issue labelled supervisor")
	}
	plan, rev, err := e.readPlan(ctx, p, parent)
	if err != nil {
		return core.Supervisor{}, err
	}
	v := core.Supervisor{ID: core.ID(), ProjectID: p.ID, Parent: parent, Plan: plan, Revision: rev, State: "queued", Evidence: map[int]core.ChildEvidence{}}
	for _, c := range plan.Children {
		i, err := e.GitHub.Issue(ctx, p, c.Issue)
		if err != nil {
			return v, err
		}
		if i.ID == 0 || i.Number != c.Issue || i.Supervisor() || i.PullRequest != nil {
			return v, fmt.Errorf("#%d must be a normal issue in this repository", c.Issue)
		}
		owner, err := e.Store.SupervisorOwner(p.ID, i.ID)
		if err != nil {
			return v, err
		}
		if owner != "" {
			old, err := e.Store.SupervisorFor(p.ID, n)
			if err != nil || old.ID != owner {
				return v, fmt.Errorf("#%d belongs to another supervisor", c.Issue)
			}
		}
		v.Evidence[c.Issue] = core.ChildEvidence{IssueID: i.ID}
	}
	// Reuse identity for read-only validation of an existing enrollment.
	candidate := v
	if old, err := e.Store.SupervisorFor(p.ID, n); err == nil {
		candidate.ID = old.ID
	}
	if err := e.Store.ValidateSupervisor(candidate); err != nil {
		return v, err
	}
	return v, nil
}

// readPlan derives the plan from the parent's sub-issues and their blocked-by
// links. A parent body with the deprecated fenced YAML block keeps using it.
// API failures are returned as is; problems with the graph itself are planErrors.
func (e *Engine) readPlan(ctx context.Context, p core.Project, parent core.Issue) (core.SupervisorPlan, string, error) {
	if core.HasPlanBlock(parent.Body) {
		plan, rev, err := core.ParseSupervisor(parent.Body, parent.Number)
		return plan, rev, asPlanError(err)
	}
	rel, ok := e.GitHub.(core.IssueRelations)
	if !ok {
		return core.SupervisorPlan{}, "", planError{fmt.Errorf("GitHub adapter does not read sub-issues")}
	}
	subs, err := rel.SubIssues(ctx, p, parent.Number)
	if err != nil {
		return core.SupervisorPlan{}, "", err
	}
	if len(subs) > 100 {
		return core.SupervisorPlan{}, "", planError{fmt.Errorf("version 1 requires 1–100 children")}
	}
	blocked := map[int][]core.Issue{}
	for _, i := range subs {
		if blocked[i.Number], err = rel.BlockedBy(ctx, p, i.Number); err != nil {
			return core.SupervisorPlan{}, "", err
		}
	}
	plan, rev, err := core.NativePlan(p.Repo, parent.Number, subs, blocked)
	return plan, rev, asPlanError(err)
}

// planError is a problem with the graph on GitHub, as opposed to a failure to read it.
type planError struct{ error }

func asPlanError(err error) error {
	if err == nil {
		return nil
	}
	return planError{err}
}

// planChangePrefix marks a hold that the next reconciliation re-evaluates, so
// fixing the sub-issues or blocked-by links on GitHub releases it on its own.
const planChangePrefix = "plan change rejected: "

func planHold(v core.Supervisor) bool {
	return v.Hold == "attention" && strings.HasPrefix(v.Reason, planChangePrefix)
}

// adoptPlan switches an enrolled supervisor to a changed graph. Edits that would
// orphan or re-police dispatched work are refused; the plan holds until they are undone.
func (e *Engine) adoptPlan(ctx context.Context, p core.Project, v *core.Supervisor, plan core.SupervisorPlan, revision string) error {
	as, err := core.List[core.Attempt](e.Store, "attempt")
	if err != nil {
		return err
	}
	fresh := *v
	fresh.Plan = plan
	fresh.Revision = revision
	fresh.Evidence = map[int]core.ChildEvidence{}
	for _, old := range v.Plan.Children {
		a := latestChild(as, p.ID, old.Issue)
		dispatched := a != nil && a.SupervisorID == v.ID
		c, kept := fresh.Child(old.Issue)
		if !kept && dispatched {
			return planError{fmt.Errorf("child #%d was dispatched; restore it to the plan", old.Issue)}
		}
		if kept && dispatched && c.Completion != old.Completion {
			return planError{fmt.Errorf("cannot change the completion policy of dispatched child #%d", old.Issue)}
		}
		// A new blocker cannot be satisfied retroactively by work already under way.
		if kept && dispatched && a.State != "done" {
			for _, d := range c.DependsOn {
				if !slices.Contains(old.DependsOn, d) {
					return planError{fmt.Errorf("cannot add blocker #%d to dispatched child #%d", d, old.Issue)}
				}
			}
		}
		if kept {
			fresh.Evidence[old.Issue] = v.Evidence[old.Issue]
		}
	}
	for _, c := range plan.Children {
		if _, ok := fresh.Evidence[c.Issue]; ok {
			continue
		}
		i, err := e.GitHub.Issue(ctx, p, c.Issue)
		if err != nil {
			return err
		}
		if i.ID == 0 || i.Number != c.Issue || i.Supervisor() || i.PullRequest != nil {
			return planError{fmt.Errorf("#%d must be a normal issue in this repository", c.Issue)}
		}
		fresh.Evidence[c.Issue] = core.ChildEvidence{IssueID: i.ID}
	}
	// Enrollment refuses children owned elsewhere or with conflicting attempts.
	if err := e.Store.EnrollSupervisor(fresh); err != nil {
		return planError{err}
	}
	_ = e.Store.Event(v.ID, "supervisor_plan", fmt.Sprintf("adopted plan %s", revision[:12]))
	*v = fresh
	return nil
}

// closureMatches ties a closed event to the issue's closed_at. GitHub records
// the two separately and they can differ by a second or so.
func closureMatches(event, closedAt time.Time) bool {
	d := event.Sub(closedAt)
	return !event.IsZero() && d >= -5*time.Second && d <= 5*time.Second
}

func latestChild(as []core.Attempt, p string, n int) *core.Attempt {
	var latest *core.Attempt
	for _, a := range as {
		if a.ProjectID == p && a.Issue.Number == n && (latest == nil || a.Generation > latest.Generation) {
			copy := a
			latest = &copy
		}
	}
	return latest
}

// evidenceFor returns the operational evidence for child n's delivery at head:
// operator-supplied evidence for that head, or else what the merged PR carries.
// The result is cached on v and "" means the delivery has none.
func (e *Engine) evidenceFor(ctx context.Context, p core.Project, v core.Supervisor, n, pr int, head string) (string, error) {
	ev := v.Evidence[n]
	if ev.EvidenceURL != "" && ev.EvidenceHead == head {
		return ev.EvidenceURL, nil
	}
	finder, ok := e.GitHub.(core.EvidenceReportFinder)
	if pr == 0 || !ok {
		return "", nil
	}
	found, err := finder.EvidenceReport(ctx, p, pr)
	if err != nil || found == "" {
		return "", err
	}
	ev.EvidenceURL, ev.EvidenceHead, ev.EvidenceSource = found, head, "pr"
	v.Evidence[n] = ev
	return found, nil
}

func missingEvidence(pr int) string {
	if pr == 0 {
		return "no operational evidence; record it with supervisor reconcile --evidence URL"
	}
	return fmt.Sprintf("PR #%d has no operational evidence (a report under %s or an \"Evidence: https://…\" line); rework the issue or use supervisor reconcile --evidence URL", pr, core.EvidenceDir)
}

func (e *Engine) completed(ctx context.Context, p core.Project, v core.Supervisor, c core.SupervisorChild, i core.Issue, a *core.Attempt) (bool, int, string, error) {
	if i.State == "open" {
		return false, 0, "issue remains open", nil
	}
	if i.State != "closed" || i.StateReason != "completed" || i.ClosedAt.IsZero() {
		return false, 0, "issue must be closed as completed with a known closure time", nil
	}
	g, err := e.supervisorAdapter()
	if err != nil {
		return false, 0, "", err
	}
	ev := v.Evidence[c.Issue]
	prnum := ev.PR
	head := ""
	if a != nil && a.SupervisorID == v.ID && a.PR != 0 {
		prnum = a.PR
	}
	if c.Completion == "merged_and_closed" {
		if prnum == 0 {
			return false, 0, "merged PR evidence is missing; use supervisor reconcile --pr", nil
		}
		pr, err := g.Delivery(ctx, p, prnum)
		if err != nil {
			return false, prnum, "", err
		}
		if pr.Number != prnum || pr.MergedAt.IsZero() || pr.Base.Ref != p.Base || pr.Base.Repo.ID != p.RepoID {
			return false, prnum, "PR must be merged into the registered repository/base", nil
		}
		if i.ClosedAt.Before(pr.MergedAt) {
			return false, prnum, "issue was closed before PR delivery; reopen and review after delivery", nil
		}
		head = pr.Head.SHA
		if !ev.External {
			if a == nil || a.SupervisorID != v.ID || a.Head == "" || a.Head != head || a.BaseSHA == "" || (a.State != "awaiting_review" && a.State != "done") {
				return false, prnum, "managed delivery has not passed its review gate", nil
			}
		}
	}
	operator, err := g.Operator(ctx)
	if err != nil {
		return false, prnum, "", err
	}
	// The human closing the issue as completed is the approval. Evidence is a
	// property of the delivery, checked here rather than approved separately.
	if c.RequiresEvidence {
		found, err := e.evidenceFor(ctx, p, v, c.Issue, prnum, head)
		if err != nil {
			return false, prnum, "", err
		}
		if found == "" {
			return false, prnum, missingEvidence(prnum), nil
		}
	}
	// An explicit acknowledgement stands in for an ambiguous (bot or automatic) closure.
	ack := ev.AcknowledgedBy == operator && ev.AckClosedAt.Equal(i.ClosedAt) && ev.AckUpdatedAt.Equal(i.UpdatedAt) && ev.AckHead == head
	if !ack {
		closure, err := g.Closure(ctx, p, i.Number)
		if err != nil {
			return false, prnum, "", err
		}
		if closure.Event != "closed" || !closureMatches(closure.CreatedAt, i.ClosedAt) || closure.Actor.Login != operator || closure.Actor.Type != "User" || closure.CommitID != "" || closure.App != nil {
			return false, prnum, "human completion is ambiguous; acknowledge completion explicitly", nil
		}
	}
	return true, prnum, "", nil
}

// inspectSupervisor uses only current remote evidence. Stored statuses are display
// data, never proof that a prerequisite is still complete.
func (e *Engine) inspectSupervisor(ctx context.Context, p core.Project, v *core.Supervisor) (map[int]core.Issue, error) {
	parent, err := e.GitHub.Issue(ctx, p, v.Parent.Number)
	if err != nil {
		return nil, err
	}
	if parent.ID != v.Parent.ID {
		return nil, fmt.Errorf("parent identity changed")
	}
	v.Parent = parent
	if !parent.Supervisor() {
		v.State = "paused"
		v.Hold = "paused"
		v.Reason = "parent supervisor label removed"
		return nil, nil
	}
	// The graph lives on GitHub and is re-read every pass. An unreadable or
	// unsafe change holds the plan until it is fixed there.
	plan, revision, err := e.readPlan(ctx, p, parent)
	if err == nil && revision != v.Revision {
		err = e.adoptPlan(ctx, p, v, plan, revision)
	}
	var invalid planError
	if errors.As(err, &invalid) {
		v.State = "needs_attention"
		v.Hold = "attention"
		v.Reason = planChangePrefix + err.Error()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	as, err := core.List[core.Attempt](e.Store, "attempt")
	if err != nil {
		return nil, err
	}
	issues := map[int]core.Issue{}
	done := map[int]bool{}
	v.Children = nil
	for _, c := range v.Plan.Children {
		i, err := e.GitHub.Issue(ctx, p, c.Issue)
		if err != nil {
			return nil, err
		}
		if i.ID != v.Evidence[c.Issue].IssueID || i.Supervisor() {
			return nil, fmt.Errorf("child #%d identity/type changed", c.Issue)
		}
		issues[c.Issue] = i
		a := latestChild(as, p.ID, c.Issue)
		ok, pr, reason, err := e.completed(ctx, p, *v, c, i, a)
		if err != nil {
			return nil, err
		}
		done[c.Issue] = ok
		status := core.ChildStatus{Issue: c.Issue, Title: i.Title, State: "pending", Reason: reason, PR: pr}
		if a != nil && a.SupervisorID == v.ID {
			status.AttemptID = a.ID
			status.State = a.State
			if status.PR == 0 {
				status.PR = a.PR
			}
			if a.State == "done" && !ok {
				v.Hold = "attention"
				v.Reason = fmt.Sprintf("previously completed child #%d no longer satisfies its gate", c.Issue)
			}
		}
		if !ok && c.RequiresEvidence && a != nil && a.SupervisorID == v.ID && a.State == "awaiting_review" {
			found, err := e.evidenceFor(ctx, p, *v, c.Issue, a.PR, a.Head)
			if err != nil {
				return nil, err
			}
			if found == "" {
				status.Reason = missingEvidence(a.PR)
			}
		}
		if ok {
			status.State = "completed"
			status.Reason = ""
		} else if i.State == "closed" {
			status.State = "needs_attention"
		}
		v.Children = append(v.Children, status)
	}
	for _, c := range v.Plan.Children {
		a := latestChild(as, p.ID, c.Issue)
		if a != nil && a.SupervisorID == v.ID && a.State != "done" {
			for _, d := range c.DependsOn {
				if !done[d] {
					v.Hold = "attention"
					v.Reason = fmt.Sprintf("dependency #%d of dispatched child #%d is no longer complete", d, c.Issue)
				}
			}
		}
	}
	if v.Hold == "attention" {
		v.State = "needs_attention"
		return issues, nil
	}
	allDone := true
	for _, c := range v.Plan.Children {
		if !done[c.Issue] {
			allDone = false
		}
	}
	if parent.State != "open" {
		if parent.StateReason == "not_planned" {
			v.State = "cancelled"
			v.Hold = "cancelled"
			v.Reason = "parent closed as not planned"
			return issues, nil
		}
		afterChildren := true
		for _, i := range issues {
			if parent.ClosedAt.Before(i.ClosedAt) {
				afterChildren = false
			}
		}
		if allDone && afterChildren && parent.StateReason == "completed" {
			g, err := e.supervisorAdapter()
			if err != nil {
				return nil, err
			}
			operator, err := g.Operator(ctx)
			if err != nil {
				return nil, err
			}
			cl, err := g.Closure(ctx, p, parent.Number)
			if err != nil {
				return nil, err
			}
			if cl.Event == "closed" && cl.Actor.Login == operator && cl.Actor.Type == "User" && cl.CommitID == "" && cl.App == nil && closureMatches(cl.CreatedAt, parent.ClosedAt) {
				v.State = "done"
				v.Current = 0
				v.Reason = "all children and parent reviewed"
				return issues, nil
			}
		}
		v.State = "needs_attention"
		v.Hold = "attention"
		v.Reason = "parent closed before verified human completion"
		return issues, nil
	}
	if allDone {
		v.State = "awaiting_parent_review"
		v.Current = 0
		v.Reason = "all children complete; close the parent after review"
		return issues, nil
	}
	// Every child whose prerequisites are satisfied is selected. Worker capacity is
	// limited by the global and project concurrency settings, not by the plan.
	// A failed child blocks only its own descendants; other branches continue.
	v.Ready = nil
	var running, review, blocked []int
	var notes []string
	for _, c := range v.Plan.Children {
		if done[c.Issue] {
			continue
		}
		a := latestChild(as, p.ID, c.Issue)
		if issues[c.Issue].State == "closed" {
			reason := childStatus(*v, c.Issue).Reason
			v.Current = c.Issue
			v.State = "needs_attention"
			v.Hold = "attention"
			v.Reason = fmt.Sprintf("#%d: %s", c.Issue, reason)
			return issues, nil
		}
		if a != nil && a.SupervisorID == v.ID {
			switch {
			case a.State == "awaiting_review":
				review = append(review, c.Issue)
				notes = append(notes, fmt.Sprintf("waiting for human completion of #%d", c.Issue))
			case a.State == "needs_attention":
				blocked = append(blocked, c.Issue)
				notes = append(notes, fmt.Sprintf("#%d: %s", c.Issue, a.Error))
			case a.State == "cancelled" && a.Error == "supervisor interrupted":
				v.Ready = append(v.Ready, c.Issue)
				notes = append(notes, fmt.Sprintf("resume interrupted child #%d", c.Issue))
			case a.State == "cancelled":
				blocked = append(blocked, c.Issue)
				notes = append(notes, fmt.Sprintf("child #%d cancelled; explicitly retry it", c.Issue))
			default:
				running = append(running, c.Issue)
				notes = append(notes, fmt.Sprintf("#%d: %s", c.Issue, a.State))
			}
			continue
		}
		ready := true
		for _, d := range c.DependsOn {
			if !done[d] {
				ready = false
			}
		}
		if !ready {
			continue
		}
		if c.Completion == "closed_completed" {
			review = append(review, c.Issue)
			notes = append(notes, fmt.Sprintf("manual completion required for #%d (no code delivery)", c.Issue))
			continue
		}
		v.Ready = append(v.Ready, c.Issue)
		notes = append(notes, fmt.Sprintf("next child #%d", c.Issue))
	}
	v.Current = 0
	for _, group := range [][]int{v.Ready, running, review, blocked} {
		if len(group) > 0 {
			v.Current = group[0]
			break
		}
	}
	v.Reason = strings.Join(notes, "; ")
	switch {
	case len(blocked) > 0:
		v.State = "needs_attention"
	case len(v.Ready) > 0:
		v.State = "active"
	case len(running) > 0:
		v.State = "waiting_child"
	case len(review) > 0:
		v.State = "waiting_review"
	default:
		v.State = "waiting_child"
		v.Reason = "waiting for dependencies"
	}
	return issues, nil
}
func childStatus(v core.Supervisor, n int) core.ChildStatus {
	for _, c := range v.Children {
		if c.Issue == n {
			return c
		}
	}
	return core.ChildStatus{}
}

func (e *Engine) stopSupervisor(v core.Supervisor) {
	as, _ := core.List[core.Attempt](e.Store, "attempt")
	for _, a := range as {
		if a.SupervisorID != v.ID || !a.Active() {
			continue
		}
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error {
			if x.Active() {
				x.State = "cancelled"
				x.Error = "supervisor interrupted"
			}
			return nil
		})
		e.mu.Lock()
		if w := e.workers[a.ID]; w != nil {
			w.cancel()
		}
		e.mu.Unlock()
	}
}
func (e *Engine) saveSupervisor(ctx context.Context, p core.Project, v *core.Supervisor) error {
	v.Updated = core.Now()
	if err := e.Store.Put("supervisor", v.ID, v); err != nil {
		return err
	}
	source := "sub-issues and their blocked-by links"
	if v.Plan.Source == core.PlanBody {
		source = "the `changathi-supervisor` block in this issue (deprecated; move it to sub-issues)"
	}
	body := fmt.Sprintf("## Changathi supervisor\n\nPlan `%s` from %s\n\nState: **%s**. %s\n\n| Child | Dependencies | State | PR | Detail |\n| --- | --- | --- | --- | --- |\n", v.Revision[:12], source, v.State, v.Reason)
	clean := func(s string) string { return strings.NewReplacer("|", "/", "\n", " ", "\r", " ").Replace(s) }
	for _, c := range v.Plan.Children {
		status := childStatus(*v, c.Issue)
		deps := []string{}
		for _, d := range c.DependsOn {
			deps = append(deps, fmt.Sprintf("#%d", d))
		}
		pr := ""
		if status.PR != 0 {
			pr = fmt.Sprintf("#%d", status.PR)
		}
		detail := status.Reason
		if ev := v.Evidence[c.Issue]; ev.EvidenceURL != "" {
			detail += " Evidence: " + ev.EvidenceURL
		}
		body += fmt.Sprintf("| #%d | %s | %s | %s | %s |\n", c.Issue, strings.Join(deps, ", "), status.State, pr, clean(detail))
	}
	body += "\nReview each delivered child and close it as completed to let its dependants start.\n"

	hash := core.SupervisorHash(body)
	if hash == v.PublishedHash || v.WorkpadRetryAt.After(core.Now()) {
		return nil
	}
	g, err := e.supervisorAdapter()
	if err != nil {
		return err
	}
	// A stable marker in the adapter recovers a previously created comment after a lost response.
	op := core.Operation{ID: v.ID + "/workpad/" + hash, Kind: "supervisor_workpad", State: "intent"}
	if err := e.Store.Put("operation", op.ID, op); err != nil {
		return err
	}
	id, err := g.SupervisorWorkpad(ctx, p, v.Parent.Number, v.ID, body)
	if err != nil {
		op.State = "uncertain"
		op.Result = err.Error()
		v.WorkpadError = err.Error()
		v.WorkpadRetryAt = core.Now().Add(2 * time.Minute)
	} else {
		op.State = "complete"
		v.CommentID = id
		v.PublishedHash = hash
		v.WorkpadError = ""
		v.WorkpadRetryAt = time.Time{}
	}
	if err := e.Store.Put("operation", op.ID, op); err != nil {
		return err
	}
	return e.Store.Put("supervisor", v.ID, v)
}
func (e *Engine) reconcileSupervisor(ctx context.Context, p core.Project, v core.Supervisor) error {
	if v.Hold != "" && !planHold(v) {
		e.stopSupervisor(v)
		return e.saveSupervisor(ctx, p, &v)
	}
	// Recompute plan-change holds from current GitHub state. An API error
	// must remain retryable rather than turning this into an unrelated hold.
	if planHold(v) {
		v.Hold = ""
	}
	if !p.Enabled {
		v.State = "paused"
		v.Reason = "project is paused"
		e.stopSupervisor(v)
		return e.saveSupervisor(ctx, p, &v)
	}
	if v.State == "done" {
		i, err := e.GitHub.Issue(ctx, p, v.Parent.Number)
		if err != nil {
			return err
		}
		if i.State == "open" {
			v.State = "needs_attention"
			v.Hold = "attention"
			v.Reason = "completed parent reopened; explicit reconciliation required"
		}
		if v.Hold != "" {
			return e.saveSupervisor(ctx, p, &v)
		}
	}
	if v.RetryAt.After(core.Now()) {
		return nil
	}
	issues, err := e.inspectSupervisor(ctx, p, &v)
	if err != nil {
		v.RemoteFailures++
		exponent := v.RemoteFailures
		if exponent > 4 {
			exponent = 4
		}
		v.RetryAt = core.Now().Add(time.Duration(1<<exponent) * 15 * time.Second)
		v.State = "waiting_remote"
		v.Reason = err.Error()
		e.stopSupervisor(v)
		return e.saveSupervisor(ctx, p, &v)
	}
	v.RetryAt = time.Time{}
	v.RemoteFailures = 0
	if v.Hold != "" {
		e.stopSupervisor(v)
	}
	// Mark completed owned attempts once the stronger supervisor evidence gate passes.
	for _, s := range v.Children {
		if s.State == "completed" && s.AttemptID != "" {
			_ = e.Store.UpdateAttempt(s.AttemptID, func(a *core.Attempt) error { a.State = "done"; return nil })
		}
	}
	if err := e.Store.Put("supervisor", v.ID, v); err != nil {
		return err
	}
	if v.Hold == "" && len(v.Ready) > 0 {
		as, err := core.List[core.Attempt](e.Store, "attempt")
		if err != nil {
			return err
		}
		var queued []string
		for _, n := range v.Ready {
			old := latestChild(as, p.ID, n)
			retry := old != nil && old.SupervisorID == v.ID && old.State == "cancelled" && old.Error == "supervisor interrupted"
			// Wait for a cancelled provider process to exit before reusing its generation.
			e.mu.Lock()
			busy := false
			if old != nil {
				_, busy = e.workers[old.ID]
			}
			e.mu.Unlock()
			if busy {
				continue
			}
			a, err := e.Store.EnqueueSupervised(p, issues[n], v, retry)
			if err != nil {
				v.State = "needs_attention"
				v.Hold = "attention"
				v.Reason = err.Error()
				break
			}
			queued = append(queued, fmt.Sprintf("#%d queued (%s)", n, a.ID))
		}
		if len(queued) > 0 && v.State == "active" {
			v.State = "waiting_capacity"
			v.Reason = strings.Join(queued, "; ")
		}
	}
	return e.saveSupervisor(ctx, p, &v)
}
func (e *Engine) pollSupervisors(ctx context.Context) {
	e.supervisorMu.Lock()
	defer e.supervisorMu.Unlock()
	ps, err := core.List[core.Project](e.Store, "project")
	if err != nil {
		return
	}
	for _, p := range ps {
		if p.Enabled && p.Auto {
			issues, err := e.GitHub.Issues(ctx, p, false)
			if err != nil {
				_ = e.Store.Event("", "supervisor_error", err.Error())
			}
			for _, i := range issues {
				if !i.Supervisor() {
					continue
				}
				if _, err := e.Store.SupervisorFor(p.ID, i.Number); err == nil {
					continue
				}
				v, err := e.loadSupervisor(ctx, p, i.Number)
				if err == nil {
					v.Trigger = "auto"
					err = e.Store.EnrollSupervisor(v)
				}
				if err != nil {
					_ = e.Store.Event("", "supervisor_error", fmt.Sprintf("%s#%d: %s", p.Name, i.Number, err))
					continue
				}
			}
		}
		plans, err := core.List[core.Supervisor](e.Store, "supervisor")
		if err != nil {
			continue
		}
		for _, v := range plans {
			if v.ProjectID == p.ID {
				if err := e.reconcileSupervisor(ctx, p, v); err != nil {
					_ = e.Store.Event(v.ID, "supervisor_error", err.Error())
				}
			}
		}
	}
}
func (e *Engine) supervisorGate(ctx context.Context, p core.Project, a core.Attempt) error {
	e.supervisorMu.Lock()
	defer e.supervisorMu.Unlock()
	return e.supervisorGateLocked(ctx, p, a)
}
func (e *Engine) supervisorGateLocked(ctx context.Context, p core.Project, a core.Attempt) error {
	owner, err := e.Store.SupervisorOwner(p.ID, a.Issue.ID)
	if err != nil {
		return err
	}
	if owner == "" && a.SupervisorID == "" {
		return nil
	}
	if owner == "" || owner != a.SupervisorID {
		return fmt.Errorf("child must be dispatched by its supervisor")
	}
	var v core.Supervisor
	if err = e.Store.Get("supervisor", owner, &v); err != nil {
		return err
	}
	if (v.Hold != "" && !planHold(v)) || v.State == "done" {
		return fmt.Errorf("supervisor is paused or complete")
	}
	v.Hold = ""
	issues, err := e.inspectSupervisor(ctx, p, &v)
	if err != nil {
		return err
	}
	if v.Hold != "" || v.Parent.State != "open" || issues[a.Issue.Number].State != "open" {
		return fmt.Errorf("supervisor gate: %s", v.Reason)
	}
	c, ok := v.Child(a.Issue.Number)
	if !ok {
		return fmt.Errorf("#%d is no longer in the plan", a.Issue.Number)
	}
	for _, dep := range c.DependsOn {
		if childStatus(v, dep).State != "completed" {
			return fmt.Errorf("dependency #%d is incomplete", dep)
		}
	}
	return nil
}

func (e *Engine) supervisorAction(ctx context.Context, p core.Project, r Request) (any, error) {
	e.supervisorMu.Lock()
	defer e.supervisorMu.Unlock()
	action := strings.TrimPrefix(r.Action, "supervisor.")
	if action == "validate" {
		v, err := e.loadSupervisor(ctx, p, r.Number)
		if err != nil {
			return nil, err
		}
		return map[string]any{"revision": v.Revision, "plan": v.Plan, "valid": true}, nil
	}
	if action == "start" {
		if !p.Enabled {
			return nil, fmt.Errorf("project is paused")
		}
		if old, err := e.Store.SupervisorFor(p.ID, r.Number); err == nil {
			// Restarting a cancelled or finished plan is explicit; the next pass
			// re-reads its graph from GitHub.
			if old.Hold != "cancelled" && old.State != "done" {
				return nil, fmt.Errorf("already enrolled (%s); use resume", old.State)
			}
			old.Hold = ""
			old.State = "queued"
			old.Reason = "operator restarted"
			old.RetryAt = time.Time{}
			if err = e.reconcileSupervisor(ctx, p, old); err != nil {
				return nil, err
			}
			e.Wake()
			return e.Store.SupervisorFor(p.ID, r.Number)
		}
		v, err := e.loadSupervisor(ctx, p, r.Number)
		if err != nil {
			return nil, err
		}
		v.Trigger = "manual"
		if err = e.Store.EnrollSupervisor(v); err != nil {
			return nil, err
		}
		if err = e.reconcileSupervisor(ctx, p, v); err != nil {
			return nil, err
		}
		e.Wake()
		return e.Store.SupervisorFor(p.ID, r.Number)
	}
	v, err := e.Store.SupervisorFor(p.ID, r.Number)
	if err != nil {
		return nil, err
	}
	switch action {
	case "show":
		return v, nil
	case "pause", "cancel":
		v.Hold = "paused"
		v.State = "paused"
		if action == "cancel" {
			v.Hold = "cancelled"
			v.State = "cancelled"
		}
		v.Reason = "operator " + action
		if err = e.Store.Put("supervisor", v.ID, v); err != nil {
			return nil, err
		}
		e.stopSupervisor(v)
	case "resume":
		if !p.Enabled {
			return nil, fmt.Errorf("project is paused")
		}
		if v.Hold == "cancelled" || v.State == "done" {
			return nil, fmt.Errorf("cancelled/completed plans restart with supervisor start")
		}
		v.Hold = ""
		v.State = "queued"
		v.Reason = "operator resumed"
		v.RetryAt = time.Time{}
		if err = e.Store.Put("supervisor", v.ID, v); err != nil {
			return nil, err
		}
		if err = e.reconcileSupervisor(ctx, p, v); err != nil {
			return nil, err
		}
		e.Wake()
		return e.Store.SupervisorFor(p.ID, r.Number)
	case "reconcile":
		c, ok := v.Child(r.Child)
		if !ok {
			return nil, fmt.Errorf("child is not in this plan")
		}
		if r.PR == 0 && !r.AckCompletion && r.Evidence == "" {
			return nil, fmt.Errorf("provide --pr, --evidence or --ack-completion")
		}
		i, err := e.GitHub.Issue(ctx, p, r.Child)
		if err != nil {
			return nil, err
		}
		ev := v.Evidence[r.Child]
		if i.ID != ev.IssueID {
			return nil, fmt.Errorf("child identity changed")
		}
		g, err := e.supervisorAdapter()
		if err != nil {
			return nil, err
		}
		as, _ := core.List[core.Attempt](e.Store, "attempt")
		a := latestChild(as, p.ID, r.Child)
		managed := a != nil && a.SupervisorID == v.ID
		if r.PR > 0 {
			if managed {
				return nil, fmt.Errorf("managed child uses its recorded delivery PR")
			}
			ev.PR = r.PR
			ev.External = true
		}
		// The delivery the reconciliation applies to: its PR and merged head.
		prnum := ev.PR
		if managed {
			prnum = a.PR
		}
		head := ""
		if c.Completion == "merged_and_closed" {
			if prnum == 0 {
				return nil, fmt.Errorf("associate a merged PR first")
			}
			pr, err := g.Delivery(ctx, p, prnum)
			if err != nil {
				return nil, err
			}
			if pr.Number != prnum || pr.MergedAt.IsZero() || pr.Base.Ref != p.Base || pr.Base.Repo.ID != p.RepoID {
				return nil, fmt.Errorf("PR must be merged into project base")
			}
			head = pr.Head.SHA
		}
		if r.Evidence != "" {
			if !core.ValidEvidenceURL(r.Evidence) {
				return nil, fmt.Errorf("evidence must be an HTTPS URL")
			}
			// Bound to this delivery: a later generation needs its own evidence.
			ev.EvidenceURL = r.Evidence
			ev.EvidenceHead = head
			ev.EvidenceSource = "operator"
		}
		if r.AckCompletion {
			if i.State != "closed" || i.StateReason != "completed" || i.ClosedAt.IsZero() {
				return nil, fmt.Errorf("acknowledgement requires an issue closed as completed")
			}
			operator, err := g.Operator(ctx)
			if err != nil {
				return nil, err
			}
			ev.AcknowledgedBy = operator
			ev.AckClosedAt = i.ClosedAt
			ev.AckUpdatedAt = i.UpdatedAt
			ev.AckHead = head
		}
		v.Evidence[r.Child] = ev
		_ = e.Store.Event(v.ID, "supervisor_reconciliation", fmt.Sprintf("operator reconciled child #%d", r.Child))
		// Reconciliation is the operator's fix for an attention hold; re-check now
		// instead of also requiring resume. Pauses and cancellations stay.
		if v.Hold == "attention" {
			v.Hold = ""
			v.State = "queued"
			v.RetryAt = time.Time{}
			if err = e.reconcileSupervisor(ctx, p, v); err != nil {
				return nil, err
			}
			e.Wake()
			return e.Store.SupervisorFor(p.ID, r.Number)
		}
	default:
		return nil, fmt.Errorf("unknown supervisor action")
	}
	if err = e.saveSupervisor(ctx, p, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// enqueueIssue claims an issue for an operator request; trigger applies to
// unsupervised issues, since a supervisor's children always run as "supervisor".
func (e *Engine) enqueueIssue(ctx context.Context, p core.Project, i core.Issue, retry bool, trigger string) (core.Attempt, error) {
	e.supervisorMu.Lock()
	defer e.supervisorMu.Unlock()
	if i.Supervisor() {
		return core.Attempt{}, fmt.Errorf("use supervisor start for parent issues")
	}
	owner, err := e.Store.SupervisorOwner(p.ID, i.ID)
	if err != nil {
		return core.Attempt{}, err
	}
	if owner == "" {
		return e.Store.Enqueue(p, i, trigger, retry)
	}
	var v core.Supervisor
	if err = e.Store.Get("supervisor", owner, &v); err != nil {
		return core.Attempt{}, err
	}
	if c, ok := v.Child(i.Number); ok && c.Completion == "closed_completed" {
		return core.Attempt{}, fmt.Errorf("child is a manual completion gate; close it on GitHub after review")
	}
	if v.Hold == "paused" || v.Hold == "cancelled" {
		return core.Attempt{}, fmt.Errorf("resume the parent first")
	}
	// Explicit retry clears a child failure only, never graph/dependency gates.
	// Older records may still carry a plan-wide hold for this child's failure.
	if retry && v.Hold == "attention" && (strings.HasPrefix(v.Reason, fmt.Sprintf("#%d:", i.Number)) || strings.HasPrefix(v.Reason, fmt.Sprintf("child #%d cancelled", i.Number))) {
		v.Hold = ""
	}
	issues, err := e.inspectSupervisor(ctx, p, &v)
	if err != nil {
		return core.Attempt{}, err
	}
	if v.Hold != "" || issues[i.Number].State != "open" {
		return core.Attempt{}, fmt.Errorf("supervisor gate: %s", v.Reason)
	}
	if !v.Selected(i.Number) {
		as, _ := core.List[core.Attempt](e.Store, "attempt")
		a := latestChild(as, p.ID, i.Number)
		// A failed child, or a delivered one the operator sends back for rework.
		failed := a != nil && a.SupervisorID == v.ID && (a.State == "needs_attention" || a.State == "cancelled" || (retry && a.State == "awaiting_review"))
		if !failed {
			return core.Attempt{}, fmt.Errorf("supervisor gate: #%d is not ready (%s)", i.Number, v.Reason)
		}
		if !retry {
			return core.Attempt{}, fmt.Errorf("child #%d needs attention; use retry", i.Number)
		}
		v.Ready = append(v.Ready, i.Number)
	}
	if err = e.Store.Put("supervisor", v.ID, v); err != nil {
		return core.Attempt{}, err
	}
	return e.Store.EnqueueSupervised(p, i, v, retry)
}
