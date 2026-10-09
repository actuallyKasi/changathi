package service

import (
	"changathi/internal/core"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type supervisorGH struct {
	*fakeGH
	lock     sync.Mutex
	nodes    map[int]core.Issue
	closures map[int]core.Closure
	external map[int]core.DeliveryEvidence
	pads     map[string]string
	writes   int
	outage   bool
	comments []core.EvidenceComment
}

func (g *supervisorGH) Issue(_ context.Context, _ core.Project, n int) (core.Issue, error) {
	g.lock.Lock()
	defer g.lock.Unlock()
	if g.outage {
		return core.Issue{}, fmt.Errorf("GitHub unavailable")
	}
	i, ok := g.nodes[n]
	if !ok {
		return i, fmt.Errorf("missing #%d", n)
	}
	return i, nil
}
func (g *supervisorGH) Issues(_ context.Context, _ core.Project, ready bool) ([]core.Issue, error) {
	g.lock.Lock()
	defer g.lock.Unlock()
	if g.outage {
		return nil, fmt.Errorf("GitHub unavailable")
	}
	var out []core.Issue
	for _, i := range g.nodes {
		if i.State == "open" && (!ready || i.Ready()) {
			out = append(out, i)
		}
	}
	return out, nil
}
func (g *supervisorGH) Operator(context.Context) (string, error) { return "operator", nil }
func (g *supervisorGH) Closure(_ context.Context, _ core.Project, n int) (core.Closure, error) {
	g.lock.Lock()
	defer g.lock.Unlock()
	return g.closures[n], nil
}
func (g *supervisorGH) Delivery(_ context.Context, p core.Project, n int) (core.DeliveryEvidence, error) {
	g.lock.Lock()
	defer g.lock.Unlock()
	if v, ok := g.external[n]; ok {
		return v, nil
	}
	g.fakeGH.mu.Lock()
	defer g.fakeGH.mu.Unlock()
	for _, pr := range g.prs {
		if pr.Number == n {
			v := core.DeliveryEvidence{Number: n}
			v.Head.SHA = pr.Head
			v.Base.Ref = pr.Base
			v.Base.Repo.ID = p.RepoID
			if pr.State == "MERGED" {
				v.MergedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			return v, nil
		}
	}
	return core.DeliveryEvidence{}, fmt.Errorf("missing PR")
}
func (g *supervisorGH) SupervisorWorkpad(_ context.Context, _ core.Project, _ int, id, body string) (int64, error) {
	g.lock.Lock()
	defer g.lock.Unlock()
	if g.outage {
		return 0, fmt.Errorf("GitHub unavailable")
	}
	g.pads[id] = body
	g.writes++
	return 100, nil
}
func (g *supervisorGH) MarkReview(_ context.Context, _ core.Project, n int) error {
	g.lock.Lock()
	defer g.lock.Unlock()
	i := g.nodes[n]
	i.Labels = []core.Label{{Name: "agent:review"}}
	g.nodes[n] = i
	return nil
}
func (g *supervisorGH) edit(n int, fn func(*core.Issue)) {
	g.lock.Lock()
	defer g.lock.Unlock()
	i := g.nodes[n]
	fn(&i)
	g.nodes[n] = i
}
func (g *supervisorGH) close(n int, reason string) {
	g.lock.Lock()
	defer g.lock.Unlock()
	i := g.nodes[n]
	i.State = "closed"
	i.StateReason = reason
	i.ClosedAt = time.Date(2026, 1, 2, 0, 0, n, 0, time.UTC)
	i.UpdatedAt = i.ClosedAt
	g.nodes[n] = i
	cl := core.Closure{ID: int64(n), Event: "closed", CreatedAt: i.ClosedAt}
	cl.Actor.Login = "operator"
	cl.Actor.Type = "User"
	g.closures[n] = cl
}
func supervisorSetup(t *testing.T) (*Engine, *supervisorGH, core.Project) {
	t.Helper()
	e, base, ps := setup(t, 1, &fakeProvider{})
	p := ps[0]
	p.Auto = false
	e.Config.Concurrency = 1
	_ = e.Store.UpdateProject(p.ID, func(x *core.Project) error { x.Auto = false; return nil })
	g := &supervisorGH{fakeGH: base, nodes: map[int]core.Issue{}, closures: map[int]core.Closure{}, external: map[int]core.DeliveryEvidence{}, pads: map[string]string{}}
	body := "Goal\n```changathi-supervisor\nversion: 1\nchildren:\n  - issue: 3\n  - issue: 4\n    depends_on: [3]\n```"
	g.nodes[2] = core.Issue{ID: 200, Number: 2, State: "open", Body: body, Labels: []core.Label{{Name: "supervisor"}}}
	for _, n := range []int{3, 4, 5} {
		g.nodes[n] = core.Issue{ID: int64(n * 100), Number: n, State: "open", Title: fmt.Sprintf("child %d", n)}
	}
	e.GitHub = g
	return e, g, p
}
func actionSuper(t *testing.T, e *Engine, p core.Project, action string, apply bool) core.Supervisor {
	t.Helper()
	v, err := e.supervisorAction(context.Background(), p, Request{Action: "supervisor." + action, Number: 2, Apply: apply})
	if err != nil {
		t.Fatal(err)
	}
	return v.(core.Supervisor)
}
func ownedAttempts(t *testing.T, e *Engine) []core.Attempt {
	t.Helper()
	as, err := core.List[core.Attempt](e.Store, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	return as
}
func TestSupervisorSerialHumanReviewAndRestart(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	e.poll(ctx)
	if len(ownedAttempts(t, e)) != 0 {
		t.Fatal("auto false dispatched")
	}
	v := actionSuper(t, e, p, "start", false)
	as := ownedAttempts(t, e)
	if len(as) != 1 || as[0].Issue.Number != 3 {
		t.Fatal(as)
	}
	e.dispatch(ctx)
	e.wg.Wait()
	as = ownedAttempts(t, e)
	if as[0].State != "awaiting_review" {
		t.Fatal(as[0])
	}
	e.poll(ctx)
	if len(ownedAttempts(t, e)) != 1 {
		t.Fatal("advanced before human review")
	}
	g.close(3, "completed")
	// A new engine uses the same durable store and reconciles instead of repeating child 3.
	e = New(e.Config, e.Store, g, &fakeProvider{})
	e.poll(ctx)
	as = ownedAttempts(t, e)
	if len(as) != 2 || as[0].State != "done" || as[1].Issue.Number != 4 {
		t.Fatal(as)
	}
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Current != 4 {
		t.Fatal(v)
	}
	e.mu.Lock()
	count := len(e.workers)
	e.mu.Unlock()
	if count != 0 {
		t.Fatal("supervisor occupied worker slot")
	}
	if _, err := e.enqueueIssue(ctx, p, g.nodes[2], false); err == nil {
		t.Fatal("parent dispatched as worker")
	}
	if _, err := e.Store.Enqueue(p, g.nodes[4], "auto", false); err == nil {
		t.Fatal("owned child bypassed gate")
	}
}
func TestSupervisorReloadAddsChildAndResume(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	actionSuper(t, e, p, "start", false)
	g.edit(2, func(i *core.Issue) {
		i.Body = strings.TrimSuffix(i.Body, "```") + "  - issue: 5\n    depends_on: [4]\n```"
	})
	e.poll(ctx)
	v, _ := e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "attention" {
		t.Fatal(v)
	}
	preview, err := e.supervisorAction(ctx, p, Request{Action: "supervisor.reload", Number: 2})
	if err != nil || preview == nil {
		t.Fatal(preview, err)
	}
	before := v.Revision
	v = actionSuper(t, e, p, "reload", true)
	if before == v.Revision || len(v.Plan.Children) != 3 || v.Hold != "paused" {
		t.Fatal(v)
	}
	v = actionSuper(t, e, p, "resume", false)
	if v.Hold != "" || v.Current != 3 {
		t.Fatal(v)
	}
	as := ownedAttempts(t, e)
	if len(as) != 1 || as[0].Generation != 1 || as[0].PlanRevision != v.Revision {
		t.Fatal(as)
	}
}
func TestSupervisorClosureFailures(t *testing.T) {
	for _, mode := range []string{"not_planned", "missing_merge", "bot", "automatic", "missing_reason", "operational"} {
		t.Run(mode, func(t *testing.T) {
			e, g, p := supervisorSetup(t)
			ctx := context.Background()
			if mode == "operational" {
				g.edit(2, func(i *core.Issue) {
					i.Body = strings.Replace(i.Body, "  - issue: 3", "  - issue: 3\n    requires_evidence: true", 1)
				})
			}
			actionSuper(t, e, p, "start", false)
			e.dispatch(ctx)
			e.wg.Wait()
			g.close(3, "completed")
			switch mode {
			case "not_planned":
				g.edit(3, func(i *core.Issue) { i.StateReason = "not_planned" })
			case "missing_reason":
				g.edit(3, func(i *core.Issue) { i.StateReason = "" })
			case "missing_merge":
				g.fakeGH.mu.Lock()
				for b, pr := range g.prs {
					pr.State = "OPEN"
					g.prs[b] = pr
				}
				g.fakeGH.mu.Unlock()
			case "bot":
				cl := g.closures[3]
				cl.Actor.Type = "Bot"
				g.closures[3] = cl
			case "automatic":
				cl := g.closures[3]
				cl.CommitID = "closing-commit"
				g.closures[3] = cl
			}
			e.poll(ctx)
			v, _ := e.Store.SupervisorFor(p.ID, 2)
			if v.State != "needs_attention" || len(ownedAttempts(t, e)) != 1 {
				t.Fatal(v)
			}
			if mode == "operational" || mode == "bot" || mode == "automatic" {
				_, err := e.supervisorAction(ctx, p, Request{Action: "supervisor.reconcile", Number: 2, Child: 3, AckCompletion: true, Evidence: "https://example.test/verification"})
				if err != nil {
					t.Fatal(err)
				}
				actionSuper(t, e, p, "resume", false)
				if len(ownedAttempts(t, e)) != 2 {
					t.Fatal("acknowledgement did not release next child")
				}
			}
		})
	}
}
func TestSupervisorReopenedDependencyBlocksDelivery(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	actionSuper(t, e, p, "start", false)
	e.dispatch(ctx)
	e.wg.Wait()
	g.close(3, "completed")
	e.poll(ctx)
	as := ownedAttempts(t, e)
	if len(as) != 2 {
		t.Fatal(as)
	}
	g.edit(3, func(i *core.Issue) { i.State = "open"; i.ClosedAt = time.Time{} })
	if err := e.eligible(ctx, p, as[1]); err == nil {
		t.Fatal("reopened prerequisite passed delivery gate")
	}
	e.poll(ctx)
	v, _ := e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "attention" {
		t.Fatal(v)
	}
}
func TestSupervisorPauseCancelAndOutage(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	actionSuper(t, e, p, "start", false)
	v := actionSuper(t, e, p, "pause", false)
	as := ownedAttempts(t, e)
	if v.Hold != "paused" || as[0].State != "cancelled" {
		t.Fatal(v, as)
	}
	if _, err := e.enqueueIssue(ctx, p, g.nodes[3], true); err == nil {
		t.Fatal("manual retry bypassed pause")
	}
	actionSuper(t, e, p, "resume", false)
	g.outage = true
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.State != "waiting_remote" {
		t.Fatal(v)
	}
	e.dispatch(ctx)
	if len(e.workers) != 0 {
		t.Fatal("outage consumed slot")
	}
	g.outage = false
	_ = e.Store.Get("supervisor", v.ID, &v)
	v.RetryAt = time.Time{}
	_ = e.Store.Put("supervisor", v.ID, v)
	e.poll(ctx)
	if len(ownedAttempts(t, e)) != 1 {
		t.Fatal("outage duplicated attempt")
	}
	actionSuper(t, e, p, "cancel", false)
	e.poll(ctx)
	if _, err := e.supervisorAction(ctx, p, Request{Action: "supervisor.resume", Number: 2}); err == nil {
		t.Fatal("cancel implicitly resumed")
	}
}
func TestSupervisorAutoDiscoveryAndStableWorkpad(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	p.Auto = true
	_ = e.Store.UpdateProject(p.ID, func(x *core.Project) error { x.Auto = true; return nil })
	e.poll(ctx)
	e.poll(ctx)
	e.poll(ctx)
	writes := g.writes
	e.poll(ctx)
	if len(ownedAttempts(t, e)) != 1 || g.writes != writes {
		t.Fatal("repeated poll duplicated work or status comment")
	}
	actionSuper(t, e, p, "pause", false)
	e.poll(ctx)
	v, _ := e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "paused" {
		t.Fatal("auto discovery resumed paused plan")
	}
}

func TestSupervisorExternalEvidenceAndParentReview(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	// First child is pre-existing code; second is a manual/no-code completion gate.
	g.edit(2, func(i *core.Issue) {
		i.Body = strings.Replace(i.Body, "    depends_on: [3]", "    depends_on: [3]\n    completion: closed_completed", 1)
	})
	g.close(3, "completed")
	v := actionSuper(t, e, p, "start", false)
	if v.Hold != "attention" {
		t.Fatal(v)
	}
	pr := core.DeliveryEvidence{Number: 9, MergedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	pr.Head.SHA = "external-head"
	pr.Base.Ref = p.Base
	pr.Base.Repo.ID = p.RepoID
	g.external[9] = pr
	_, err := e.supervisorAction(ctx, p, Request{Action: "supervisor.reconcile", Number: 2, Child: 3, PR: 9})
	if err != nil {
		t.Fatal(err)
	}
	v = actionSuper(t, e, p, "resume", false)
	if v.State != "waiting_review" || v.Current != 4 || len(ownedAttempts(t, e)) != 0 {
		t.Fatal(v)
	}
	if _, err = e.enqueueIssue(ctx, p, g.nodes[4], false); err == nil {
		t.Fatal("manual-only gate started a code worker")
	}
	g.close(4, "completed")
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.State != "awaiting_parent_review" {
		t.Fatal(v)
	}
	g.close(2, "completed")
	// Explicitly order parent human review after its children.
	g.edit(2, func(i *core.Issue) { i.ClosedAt = i.ClosedAt.Add(time.Minute); i.UpdatedAt = i.ClosedAt })
	cl := g.closures[2]
	cl.CreatedAt = g.nodes[2].ClosedAt
	g.closures[2] = cl
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.State != "done" {
		t.Fatal(v)
	}
	g.edit(2, func(i *core.Issue) { i.State = "open" })
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "attention" {
		t.Fatal("reopened parent restarted")
	}
}
func TestSupervisorEnrollmentConflictIsAtomic(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	if _, err := e.Store.Enqueue(p, g.nodes[4], "manual", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.supervisorAction(ctx, p, Request{Action: "supervisor.validate", Number: 2}); err == nil {
		t.Fatal("validation omitted attempt conflict")
	}
	if _, err := e.supervisorAction(ctx, p, Request{Action: "supervisor.start", Number: 2}); err == nil {
		t.Fatal("adopted active external attempt")
	}
	plans, _ := core.List[core.Supervisor](e.Store, "supervisor")
	owner, _ := e.Store.SupervisorOwner(p.ID, 300)
	if len(plans) != 0 || owner != "" {
		t.Fatal("partial enrollment")
	}
}
func TestSupervisorDurablePausedState(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	actionSuper(t, e, p, "start", false)
	actionSuper(t, e, p, "pause", false)
	dir := e.Store.Dir
	_ = e.Store.Close()
	reopened, err := core.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e = New(e.Config, reopened, g, &fakeProvider{})
	e.poll(ctx)
	v, _ := e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "paused" || len(ownedAttempts(t, e)) != 1 {
		t.Fatal(v)
	}
	if _, err = e.Store.Enqueue(p, g.nodes[4], "auto", false); err == nil {
		t.Fatal("ownership lost on restart")
	}
	actionSuper(t, e, p, "resume", false)
	as := ownedAttempts(t, e)
	if len(as) != 1 || as[0].State != "queued" {
		t.Fatal(as)
	}
}
func TestSupervisorLabelRemovalAndStaleAcknowledgement(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	actionSuper(t, e, p, "start", false)
	g.edit(2, func(i *core.Issue) { i.Labels = nil })
	e.poll(ctx)
	v, _ := e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "paused" {
		t.Fatal(v)
	}
	g.edit(2, func(i *core.Issue) { i.Labels = []core.Label{{Name: "supervisor"}} })
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "paused" {
		t.Fatal("label re-add resumed plan")
	}
	actionSuper(t, e, p, "resume", false)
	e.dispatch(ctx)
	e.wg.Wait()
	g.close(3, "completed")
	cl := g.closures[3]
	cl.CommitID = "auto-close"
	g.closures[3] = cl
	e.poll(ctx)
	_, err := e.supervisorAction(ctx, p, Request{Action: "supervisor.reconcile", Number: 2, Child: 3, AckCompletion: true})
	if err != nil {
		t.Fatal(err)
	}
	g.edit(3, func(i *core.Issue) { i.UpdatedAt = i.UpdatedAt.Add(time.Minute) })
	v = actionSuper(t, e, p, "resume", false)
	if v.Hold != "attention" || len(ownedAttempts(t, e)) != 1 {
		t.Fatal("stale acknowledgement passed", v)
	}
}
func TestSupervisorConcurrentTriggers(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	actionSuper(t, e, p, "start", false)
	var wg sync.WaitGroup
	for n := 0; n < 10; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = e.enqueueIssue(ctx, p, g.nodes[3], false); e.pollSupervisors(ctx) }()
	}
	wg.Wait()
	if len(ownedAttempts(t, e)) != 1 {
		t.Fatal("duplicate generation")
	}
}

func TestSupervisorResumesInactivePriorAttempt(t *testing.T) {
	e, g, p := supervisorSetup(t)
	old, err := e.Store.Enqueue(p, g.nodes[3], "manual", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.Store.UpdateAttempt(old.ID, func(a *core.Attempt) error {
		a.State = "needs_attention"
		a.Error = "previous interruption"
		return nil
	})
	v := actionSuper(t, e, p, "start", false)
	as := ownedAttempts(t, e)
	if len(as) != 1 || as[0].ID != old.ID || as[0].SupervisorID != v.ID || as[0].State != "queued" {
		t.Fatal(as, v)
	}
}

func (g *supervisorGH) EvidenceComments(context.Context, core.Project, int) ([]core.EvidenceComment, error) {
	g.lock.Lock()
	defer g.lock.Unlock()
	return append([]core.EvidenceComment(nil), g.comments...), nil
}
func TestGitHubEvidenceApprovalAdvancesWithoutCLI(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	g.edit(2, func(i *core.Issue) {
		i.Body = strings.Replace(i.Body, "  - issue: 3", "  - issue: 3\n    requires_evidence: true", 1)
	})
	actionSuper(t, e, p, "start", false)
	e.dispatch(ctx)
	e.wg.Wait()
	g.close(3, "completed")
	e.poll(ctx)
	v, _ := e.Store.SupervisorFor(p.ID, 2)
	if !evidenceHold(v) {
		t.Fatalf("expected evidence hold: %+v", v)
	}
	a := latestChild(ownedAttempts(t, e), p.ID, 3)
	comment := core.EvidenceComment{ID: 10, Body: fmt.Sprintf("/approve-evidence #%d https://example.test/report", a.PR), CreatedAt: g.nodes[3].ClosedAt.Add(time.Second)}
	comment.UpdatedAt = comment.CreatedAt
	comment.User.Login = "operator"
	comment.User.Type = "User"
	g.comments = []core.EvidenceComment{comment}
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "" || v.Current != 4 || len(ownedAttempts(t, e)) != 2 {
		t.Fatalf("approval did not advance: %+v", v)
	}
	if v.Evidence[3].ApprovalCommentID != 10 {
		t.Fatal("missing approval audit")
	}
	// Deleting approval invalidates it even after it was persisted.
	g.comments = nil
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "attention" {
		t.Fatal("deleted approval remained valid")
	}
}
func TestEvidenceCommentValidation(t *testing.T) {
	closed := time.Now().UTC().Truncate(time.Second)
	valid := core.EvidenceComment{ID: 1, Body: "/approve-evidence #13 https://example.test/report", CreatedAt: closed.Add(time.Second), UpdatedAt: closed.Add(time.Second)}
	valid.User.Login = "operator"
	valid.User.Type = "User"
	if !validEvidenceComment(valid, "operator", 13, closed) {
		t.Fatal("valid approval rejected")
	}
	for _, mode := range []string{"other_user", "bot", "old", "edited", "wrong_pr", "http", "extra", "reopen"} {
		t.Run(mode, func(t *testing.T) {
			c := valid
			closeTime := closed
			switch mode {
			case "other_user":
				c.User.Login = "someone"
			case "bot":
				c.User.Type = "Bot"
			case "old":
				c.CreatedAt = closed.Add(-time.Second)
				c.UpdatedAt = c.CreatedAt
			case "edited":
				c.UpdatedAt = c.CreatedAt.Add(time.Second)
			case "wrong_pr":
				c.Body = "/approve-evidence #12 https://example.test/report"
			case "http":
				c.Body = "/approve-evidence #13 http://example.test/report"
			case "extra":
				c.Body += " extra"
			case "reopen":
				closeTime = c.CreatedAt.Add(time.Second)
			}
			if validEvidenceComment(c, "operator", 13, closeTime) {
				t.Fatal("invalid approval accepted")
			}
		})
	}
}

func TestApprovalPromptContainsExactCommand(t *testing.T) {
	e, g, p := supervisorSetup(t)
	v := core.Supervisor{ID: "plan", Parent: core.Issue{Number: 2}, Plan: core.SupervisorPlan{Children: []core.SupervisorChild{{Issue: 3, RequiresEvidence: true}}}, Children: []core.ChildStatus{{Issue: 3, PR: 13, State: "awaiting_review"}}}
	if err := e.publishApprovalPrompts(context.Background(), p, &v); err != nil {
		t.Fatal(err)
	}
	body := g.pads["plan/approval/3"]
	command := fmt.Sprintf("/approve-evidence #13 https://github.com/%s/pull/13", p.Repo)
	if !strings.Contains(body, command) || !strings.Contains(body, "close this issue as completed first") {
		t.Fatal(body)
	}
	writes := g.writes
	if err := e.publishApprovalPrompts(context.Background(), p, &v); err != nil {
		t.Fatal(err)
	}
	if g.writes != writes {
		t.Fatal("unchanged prompt republished")
	}
	v.Children[0].PR = 14
	if err := e.publishApprovalPrompts(context.Background(), p, &v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.pads["plan/approval/3"], "/approve-evidence #14 ") {
		t.Fatal("stale PR prompt")
	}
}
