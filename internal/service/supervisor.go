package service

import (
	"changathi/internal/core"
	"context"
	"fmt"
	"net/url"
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
	plan, rev, err := core.ParseSupervisor(parent.Body, n)
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

const evidencePending = "approve evidence on this closed issue with /approve-evidence #PR HTTPS_URL"
const legacyEvidencePending = "operational outcome requires supervisor reconcile --ack-completion --evidence URL"

func evidenceHold(v core.Supervisor) bool {
	return v.Hold == "attention" && (strings.HasSuffix(v.Reason, ": "+evidencePending) || strings.HasSuffix(v.Reason, ": "+legacyEvidencePending))
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
	ack := ev.ApprovalCommentID == 0 && ev.AcknowledgedBy == operator && ev.AckClosedAt.Equal(i.ClosedAt) && ev.AckUpdatedAt.Equal(i.UpdatedAt) && ev.AckHead == head
	if c.RequiresEvidence && (!ack || ev.EvidenceURL == "") {
		if reader, supported := e.GitHub.(core.EvidenceComments); supported {
			comments, err := reader.EvidenceComments(ctx, p, i.Number)
			if err != nil {
				return false, prnum, "", err
			}
			for _, comment := range comments {
				if !validEvidenceComment(comment, operator, prnum, i.ClosedAt) {
					continue
				}
				fields := strings.Fields(comment.Body)
				ev.EvidenceURL = fields[2]
				ev.ApprovalCommentID = comment.ID
				ev.AcknowledgedBy = operator
				ev.AckClosedAt = i.ClosedAt
				ev.AckUpdatedAt = i.UpdatedAt
				ev.AckHead = head
				v.Evidence[c.Issue] = ev
				ack = true
				break
			}
		}
		if !ack || ev.EvidenceURL == "" {
			return false, prnum, evidencePending, nil
		}
	}
	if !ack {
		closure, err := g.Closure(ctx, p, i.Number)
		if err != nil {
			return false, prnum, "", err
		}
		if closure.Event != "closed" || !closure.CreatedAt.Equal(i.ClosedAt) || closure.Actor.Login != operator || closure.Actor.Type != "User" || closure.CommitID != "" || closure.App != nil {
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
	plan, revision, err := core.ParseSupervisor(parent.Body, parent.Number)
	_ = plan
	if err != nil || revision != v.Revision {
		v.State = "needs_attention"
		v.Hold = "attention"
		v.Reason = "parent graph changed; validate and reload the plan"
		return nil, nil
	}
	v.Parent = parent
	if !parent.Supervisor() {
		v.State = "paused"
		v.Hold = "paused"
		v.Reason = "parent supervisor label removed"
		return nil, nil
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
	// Evidence approval is a per-child wait now, never a plan-wide hold.
	if evidenceHold(*v) {
		v.Hold = ""
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
			if cl.Event == "closed" && cl.Actor.Login == operator && cl.Actor.Type == "User" && cl.CommitID == "" && cl.App == nil && cl.CreatedAt.Equal(parent.ClosedAt) {
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
			if reason == evidencePending {
				review = append(review, c.Issue)
				notes = append(notes, fmt.Sprintf("#%d: %s", c.Issue, reason))
				continue
			}
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
	if !v.WorkpadRetryAt.After(core.Now()) {
		if err := e.publishApprovalPrompts(ctx, p, v); err != nil {
			v.WorkpadError = err.Error()
			v.WorkpadRetryAt = core.Now().Add(2 * time.Minute)
			return e.Store.Put("supervisor", v.ID, v)
		}
	}
	body := fmt.Sprintf("## Changathi supervisor\n\nPlan `%s`\n\nState: **%s**. %s\n\n| Child | Dependencies | State | PR | Detail |\n| --- | --- | --- | --- | --- |\n", v.Revision[:12], v.State, v.Reason)
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
	for _, c := range v.Plan.Children {
		status := childStatus(*v, c.Issue)
		if prompt, ok := v.ApprovalPrompts[c.Issue]; ok && prompt.PR == status.PR && status.State != "completed" {
			body += fmt.Sprintf("\n### Approve #%d\n\nReview [the evidence](%s), close #%d as completed, then post this exact comment **on #%d**:\n\n```text\n/approve-evidence #%d %s\n```\n\nChangathi validates your approval and advances automatically.\n", c.Issue, prompt.URL, c.Issue, c.Issue, prompt.PR, prompt.URL)
		}
	}

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
	if v.Hold != "" && !evidenceHold(v) {
		e.stopSupervisor(v)
		return e.saveSupervisor(ctx, p, &v)
	}
	// Recompute evidence-only holds from current GitHub state. An API error
	// must remain retryable rather than turning this into an unrelated hold.
	if evidenceHold(v) {
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
	if v.Hold != "" || v.State == "done" || v.Revision != a.PlanRevision {
		return fmt.Errorf("supervisor is paused, changed, or complete")
	}
	issues, err := e.inspectSupervisor(ctx, p, &v)
	if err != nil {
		return err
	}
	if v.Hold != "" || v.Parent.State != "open" || issues[a.Issue.Number].State != "open" {
		return fmt.Errorf("supervisor gate: %s", v.Reason)
	}
	for _, c := range v.Plan.Children {
		if c.Issue == a.Issue.Number {
			for _, dep := range c.DependsOn {
				if childStatus(v, dep).State != "completed" {
					return fmt.Errorf("dependency #%d is incomplete", dep)
				}
			}
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
			return nil, fmt.Errorf("already enrolled (%s); use resume or reload", old.State)
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
			return nil, fmt.Errorf("cancelled/completed plans require reload --apply before resume")
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
	case "reload":
		fresh, err := e.loadSupervisor(ctx, p, r.Number)
		if err != nil {
			return nil, err
		}
		if !r.Apply {
			return map[string]any{"current_revision": v.Revision, "proposed_revision": fresh.Revision, "current": v.Plan, "proposed": fresh.Plan, "apply_command": "supervisor reload --apply"}, nil
		}
		as, err := core.List[core.Attempt](e.Store, "attempt")
		if err != nil {
			return nil, err
		}
		for _, a := range as {
			if a.SupervisorID == v.ID && a.Active() {
				return nil, fmt.Errorf("pause the supervisor and wait for active workers before reload")
			}
		}
		e.mu.Lock()
		busy := false
		for _, a := range as {
			if a.SupervisorID == v.ID {
				if _, ok := e.workers[a.ID]; ok {
					busy = true
				}
			}
		}
		e.mu.Unlock()
		if busy {
			return nil, fmt.Errorf("worker is still stopping")
		}
		for _, old := range v.Plan.Children {
			c, kept := fresh.Child(old.Issue)
			a := latestChild(as, p.ID, old.Issue)
			if !kept {
				if a != nil {
					return nil, fmt.Errorf("cannot remove dispatched child #%d", old.Issue)
				}
			} else {
				if core.SupervisorHash(c) != core.SupervisorHash(old) && a != nil {
					return nil, fmt.Errorf("cannot change policy/dependencies of dispatched child #%d", old.Issue)
				}
				if ev := v.Evidence[old.Issue]; ev.PR != 0 || ev.AcknowledgedBy != "" {
					if core.SupervisorHash(c) != core.SupervisorHash(old) {
						return nil, fmt.Errorf("cannot change an evidenced child policy")
					}
				}
				fresh.Evidence[old.Issue] = v.Evidence[old.Issue]
			}
		}
		v.Plan = fresh.Plan
		v.Revision = fresh.Revision
		v.Evidence = fresh.Evidence
		v.Parent = fresh.Parent
		v.Hold = "paused"
		v.State = "paused"
		v.Reason = "plan reloaded; resume to dispatch"
		v.Current = 0
		v.Ready = nil
		v.Children = nil
		if err = e.Store.EnrollSupervisor(v); err != nil {
			return nil, err
		}
	case "reconcile":
		c, ok := v.Child(r.Child)
		if !ok {
			return nil, fmt.Errorf("child is not in this plan")
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
		if r.PR > 0 {
			pr, err := g.Delivery(ctx, p, r.PR)
			if err != nil {
				return nil, err
			}
			if pr.Number != r.PR || pr.MergedAt.IsZero() || pr.Base.Ref != p.Base || pr.Base.Repo.ID != p.RepoID {
				return nil, fmt.Errorf("PR must be merged into project base")
			}
			as, _ := core.List[core.Attempt](e.Store, "attempt")
			a := latestChild(as, p.ID, r.Child)
			if a != nil && a.SupervisorID == v.ID {
				return nil, fmt.Errorf("managed child uses its recorded delivery PR")
			}
			ev.PR = r.PR
			ev.External = true
		}
		if r.AckCompletion {
			if i.State != "closed" || i.StateReason != "completed" || i.ClosedAt.IsZero() {
				return nil, fmt.Errorf("acknowledgement requires an issue closed as completed")
			}
			head := ""
			prnum := ev.PR
			as, _ := core.List[core.Attempt](e.Store, "attempt")
			a := latestChild(as, p.ID, r.Child)
			if a != nil && a.SupervisorID == v.ID {
				prnum = a.PR
			}
			if c.Completion == "merged_and_closed" {
				if prnum == 0 {
					return nil, fmt.Errorf("associate a merged PR first")
				}
				pr, err := g.Delivery(ctx, p, prnum)
				if err != nil {
					return nil, err
				}
				if pr.MergedAt.IsZero() || pr.Base.Ref != p.Base || pr.Base.Repo.ID != p.RepoID || i.ClosedAt.Before(pr.MergedAt) {
					return nil, fmt.Errorf("invalid delivery evidence")
				}
				head = pr.Head.SHA
			}
			if r.Evidence != "" {
				u, err := url.Parse(r.Evidence)
				if err != nil || u.Scheme != "https" || u.Host == "" {
					return nil, fmt.Errorf("evidence must be an HTTPS URL")
				}
			}
			if c.RequiresEvidence && r.Evidence == "" {
				return nil, fmt.Errorf("this child requires an evidence URL")
			}
			operator, err := g.Operator(ctx)
			if err != nil {
				return nil, err
			}
			ev.AcknowledgedBy = operator
			ev.AckClosedAt = i.ClosedAt
			ev.AckUpdatedAt = i.UpdatedAt
			ev.AckHead = head
			ev.ApprovalCommentID = 0
			ev.EvidenceURL = r.Evidence
		}
		if r.PR == 0 && !r.AckCompletion {
			return nil, fmt.Errorf("provide --pr or --ack-completion")
		}
		v.Evidence[r.Child] = ev
		_ = e.Store.Event(v.ID, "supervisor_reconciliation", fmt.Sprintf("operator reconciled child #%d", r.Child))
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

func validEvidenceComment(c core.EvidenceComment, operator string, pr int, closed time.Time) bool {
	if pr == 0 || c.ID == 0 || c.User.Login != operator || c.User.Type != "User" || c.CreatedAt.Before(closed) || c.CreatedAt.IsZero() || !c.UpdatedAt.Equal(c.CreatedAt) {
		return false
	}
	f := strings.Fields(c.Body)
	if len(f) != 3 || f[0] != "/approve-evidence" || f[1] != fmt.Sprintf("#%d", pr) {
		return false
	}
	u, err := url.Parse(f[2])
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func (e *Engine) publishApprovalPrompts(ctx context.Context, p core.Project, v *core.Supervisor) error {
	g, err := e.supervisorAdapter()
	if err != nil {
		return err
	}
	if v.ApprovalPrompts == nil {
		v.ApprovalPrompts = map[int]core.ApprovalPrompt{}
	}
	for _, c := range v.Plan.Children {
		status := childStatus(*v, c.Issue)
		if !c.RequiresEvidence || status.PR == 0 || (status.State != "awaiting_review" && status.State != "needs_attention") {
			continue
		}
		if old, ok := v.ApprovalPrompts[c.Issue]; ok && old.PR == status.PR {
			continue
		}
		report := fmt.Sprintf("https://github.com/%s/pull/%d", p.Repo, status.PR)
		if finder, ok := e.GitHub.(core.EvidenceReportFinder); ok {
			report, err = finder.EvidenceReport(ctx, p, status.PR)
			if err != nil {
				return err
			}
		}
		body := fmt.Sprintf("## Review and approve operational evidence\n\nReview [the validation evidence](%s) delivered in PR #%d. If you accept the results, **close this issue as completed first**, then post this exact comment here:\n\n```text\n/approve-evidence #%d %s\n```\n\nPost it as the human operator in a new comment after closure. Edited comments and approvals for older PRs or previous closures are ignored. Changathi verifies the merged delivery and advances supervisor #%d automatically; no CLI acknowledgement or resume is needed unless you explicitly paused the plan.\n", report, status.PR, status.PR, report, v.Parent.Number)
		// Stable marker recovers a successful POST whose response was lost.
		if _, err := g.SupervisorWorkpad(ctx, p, c.Issue, fmt.Sprintf("%s/approval/%d", v.ID, c.Issue), body); err != nil {
			return err
		}
		v.ApprovalPrompts[c.Issue] = core.ApprovalPrompt{PR: status.PR, URL: report}
		if err := e.Store.Put("supervisor", v.ID, v); err != nil {
			return err
		}
	}
	return nil
}
