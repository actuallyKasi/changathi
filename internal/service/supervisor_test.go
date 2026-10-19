package service

import (
	"changathi/internal/core"
	"changathi/internal/provider"
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
	subs     map[int][]int
	blocked  map[int][]int
	evidence map[int]string
}

func (g *supervisorGH) relations(ns []int) ([]core.Issue, error) {
	g.lock.Lock()
	defer g.lock.Unlock()
	if g.outage {
		return nil, fmt.Errorf("GitHub unavailable")
	}
	out := []core.Issue{}
	for _, n := range ns {
		out = append(out, g.nodes[n])
	}
	return out, nil
}
func (g *supervisorGH) SubIssues(_ context.Context, _ core.Project, n int) ([]core.Issue, error) {
	return g.relations(g.subs[n])
}
func (g *supervisorGH) BlockedBy(_ context.Context, _ core.Project, n int) ([]core.Issue, error) {
	return g.relations(g.blocked[n])
}
func (g *supervisorGH) EvidenceReport(_ context.Context, _ core.Project, pr int) (string, error) {
	g.lock.Lock()
	defer g.lock.Unlock()
	return g.evidence[pr], nil
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
	g := &supervisorGH{fakeGH: base, nodes: map[int]core.Issue{}, closures: map[int]core.Closure{}, external: map[int]core.DeliveryEvidence{}, pads: map[string]string{}, subs: map[int][]int{}, blocked: map[int][]int{}, evidence: map[int]string{}}
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
func TestSupervisorHumanReviewAndRestart(t *testing.T) {
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
	e = New(e.Config, e.Store, g, map[string]provider.Provider{"codex": &fakeProvider{}})
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
	if _, err := e.enqueueIssue(ctx, p, g.nodes[2], false, "manual"); err == nil {
		t.Fatal("parent dispatched as worker")
	}
	if _, err := e.Store.Enqueue(p, g.nodes[4], "auto", false); err == nil {
		t.Fatal("owned child bypassed gate")
	}
}

// Graph edits on GitHub are adopted on the next pass, without reload or resume.
func TestSupervisorAdoptsGraphChanges(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	actionSuper(t, e, p, "start", false)
	body := g.nodes[2].Body
	g.edit(2, func(i *core.Issue) {
		i.Body = strings.TrimSuffix(i.Body, "```") + "  - issue: 5\n    depends_on: [4]\n```"
	})
	e.poll(ctx)
	v, _ := e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "" || len(v.Plan.Children) != 3 || v.Current != 3 {
		t.Fatal(v)
	}
	as := ownedAttempts(t, e)
	if len(as) != 1 || as[0].Generation != 1 {
		t.Fatal(as)
	}
	// The running child was dispatched under the earlier revision and stays eligible.
	if err := e.eligible(ctx, p, as[0]); err != nil {
		t.Fatal(err)
	}
	// Removing a dispatched child holds the plan until it is restored.
	g.edit(2, func(i *core.Issue) {
		i.Body = strings.Replace(body, "  - issue: 3\n  - issue: 4\n    depends_on: [3]", "  - issue: 4", 1)
	})
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "attention" || !strings.Contains(v.Reason, "child #3 was dispatched") {
		t.Fatal(v)
	}
	g.edit(2, func(i *core.Issue) { i.Body = body })
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "" || len(v.Plan.Children) != 2 {
		t.Fatal("restored graph did not release the hold", v)
	}
	if owner, _ := e.Store.SupervisorOwner(p.ID, 500); owner != "" {
		t.Fatal("removed child is still owned")
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
			r := Request{Action: "supervisor.reconcile", Number: 2, Child: 3}
			switch mode {
			case "operational":
				if !strings.Contains(v.Reason, "no operational evidence") {
					t.Fatal(v.Reason)
				}
				r.Evidence = "https://example.test/verification"
			case "bot", "automatic":
				r.AckCompletion = true
			default:
				return
			}
			// Reconciliation releases the hold it fixes; no resume needed.
			if _, err := e.supervisorAction(ctx, p, r); err != nil {
				t.Fatal(err)
			}
			if len(ownedAttempts(t, e)) != 2 {
				v, _ = e.Store.SupervisorFor(p.ID, 2)
				t.Fatal("reconciliation did not release next child", v)
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
	if _, err := e.enqueueIssue(ctx, p, g.nodes[3], true, "manual"); err == nil {
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
	if v := actionSuper(t, e, p, "start", false); v.Hold != "" {
		t.Fatal("start did not restart the cancelled plan", v)
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
	if _, err = e.enqueueIssue(ctx, p, g.nodes[4], false, "manual"); err == nil {
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
	e = New(e.Config, reopened, g, map[string]provider.Provider{"codex": &fakeProvider{}})
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
	if len(ownedAttempts(t, e)) != 2 {
		t.Fatal("acknowledgement did not release next child")
	}
	g.edit(3, func(i *core.Issue) { i.UpdatedAt = i.UpdatedAt.Add(time.Minute) })
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "attention" {
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
		go func() {
			defer wg.Done()
			_, _ = e.enqueueIssue(ctx, p, g.nodes[3], false, "manual")
			e.pollSupervisors(ctx)
		}()
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

// Closing the issue is the only approval; evidence comes from the delivery PR.
func TestEvidenceFromDeliveryPR(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	g.edit(2, func(i *core.Issue) {
		i.Body = strings.Replace(i.Body, "  - issue: 3", "  - issue: 3\n    requires_evidence: true", 1)
	})
	actionSuper(t, e, p, "start", false)
	e.dispatch(ctx)
	e.wg.Wait()
	a := latestChild(ownedAttempts(t, e), p.ID, 3)
	e.poll(ctx)
	v, _ := e.Store.SupervisorFor(p.ID, 2)
	// The missing evidence is visible before the operator closes the issue.
	if !strings.Contains(childStatus(v, 3).Reason, fmt.Sprintf("PR #%d has no operational evidence", a.PR)) {
		t.Fatal(childStatus(v, 3))
	}
	g.lock.Lock()
	g.evidence[a.PR] = "https://example.test/report"
	g.lock.Unlock()
	g.close(3, "completed")
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "" || v.Current != 4 || len(ownedAttempts(t, e)) != 2 {
		t.Fatalf("closure did not advance: %+v", v)
	}
	if ev := v.Evidence[3]; ev.EvidenceURL != "https://example.test/report" || ev.EvidenceSource != "pr" || ev.EvidenceHead != a.Head {
		t.Fatal(ev)
	}
	if !strings.Contains(g.pads[v.ID], "Evidence: https://example.test/report") {
		t.Fatal(g.pads[v.ID])
	}
	// Approval comments are no longer part of the gate.
	if strings.Contains(g.pads[v.ID], "/approve-evidence") {
		t.Fatal("workpad still asks for an approval comment")
	}
}

// Plans read from sub-issues and blocked-by links, with policies from labels.
func TestSupervisorNativePlan(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	g.edit(2, func(i *core.Issue) { i.Body = "Goal only; the graph is on GitHub." })
	if _, err := e.supervisorAction(ctx, p, Request{Action: "supervisor.validate", Number: 2}); err == nil {
		t.Fatal("parent without sub-issues validated")
	}
	g.edit(3, func(i *core.Issue) { i.Labels = []core.Label{{Name: core.EvidenceLabel}} })
	g.edit(5, func(i *core.Issue) { i.Labels = []core.Label{{Name: core.ManualLabel}} })
	g.subs[2] = []int{3, 4, 5}
	g.blocked[4] = []int{3}
	v := actionSuper(t, e, p, "start", false)
	c3, _ := v.Child(3)
	c4, _ := v.Child(4)
	c5, _ := v.Child(5)
	if v.Plan.Source != core.PlanSubIssues || !c3.RequiresEvidence || len(c4.DependsOn) != 1 || c5.Completion != "closed_completed" {
		t.Fatal(v.Plan)
	}
	as := ownedAttempts(t, e)
	if len(as) != 1 || as[0].Issue.Number != 3 {
		t.Fatal("manual gate or blocked child dispatched", as)
	}
	prompt, err := implementPrompt(p, as[0], core.Workflow{})
	if err != nil || !strings.Contains(prompt, core.EvidenceDir) {
		t.Fatal("evidence child prompt lacks the report instruction", err)
	}
	hold := func(want string) {
		t.Helper()
		e.poll(ctx)
		v, _ = e.Store.SupervisorFor(p.ID, 2)
		if want == "" && v.Hold != "" || want != "" && (v.Hold != "attention" || !strings.Contains(v.Reason, want)) {
			t.Fatal(want, v.Hold, v.Reason)
		}
	}
	// A blocker outside the plan, a removed dispatched child and a new blocker on
	// in-flight work each hold the plan until undone on GitHub.
	g.nodes[9] = core.Issue{ID: 900, Number: 9, State: "open"}
	g.blocked[4] = []int{3, 9}
	hold("not a sub-issue of #2")
	g.blocked[4] = []int{3}
	hold("")
	g.subs[2] = []int{4, 5}
	hold("#4 is blocked by #3, which is not a sub-issue")
	delete(g.blocked, 4)
	hold("child #3 was dispatched")
	g.subs[2] = []int{3, 4, 5}
	g.blocked[4] = []int{3}
	hold("")
	g.blocked[3] = []int{5}
	hold("cannot add blocker #5 to dispatched child #3")
	delete(g.blocked, 3)
	hold("")
	// Removing a blocker from an undispatched child takes effect at once.
	delete(g.blocked, 4)
	hold("")
	if a := latestChild(ownedAttempts(t, e), p.ID, 4); a == nil {
		t.Fatal("unblocked child did not start")
	}
	// An API outage while reading the graph is a remote wait, not a plan hold.
	g.outage = true
	e.poll(ctx)
	g.outage = false
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	if v.Hold != "" || v.State != "waiting_remote" {
		t.Fatal(v)
	}
}

func TestSupervisorRunsIndependentChildrenInParallel(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	// 3 and 5 are independent; 4 depends on 3.
	g.edit(2, func(i *core.Issue) {
		i.Body = strings.TrimSuffix(i.Body, "```") + "  - issue: 5\n```"
	})
	v := actionSuper(t, e, p, "start", false)
	as := ownedAttempts(t, e)
	if len(as) != 2 || v.State != "waiting_capacity" {
		t.Fatal(v, as)
	}
	queued := map[int]bool{}
	for _, a := range as {
		queued[a.Issue.Number] = true
	}
	if !queued[3] || !queued[5] {
		t.Fatal(as)
	}
	// A failed independent child does not hold the rest of the plan.
	a5 := latestChild(as, p.ID, 5)
	_ = e.Store.UpdateAttempt(a5.ID, func(a *core.Attempt) error { a.State = "needs_attention"; a.Error = "validation failed"; return nil })
	e.poll(ctx)
	v, _ = e.Store.SupervisorFor(p.ID, 2)
	a3 := latestChild(ownedAttempts(t, e), p.ID, 3)
	if v.Hold != "" || v.State != "needs_attention" || a3.State == "cancelled" {
		t.Fatal(v, a3)
	}
	if err := e.eligible(ctx, p, *a3); err != nil {
		t.Fatal("sibling failure blocked running child:", err)
	}
	if _, err := e.enqueueIssue(ctx, p, g.nodes[5], false, "manual"); err == nil {
		t.Fatal("failed child restarted without retry")
	}
	if _, err := e.enqueueIssue(ctx, p, g.nodes[5], true, "manual"); err != nil {
		t.Fatal(err)
	}
	if a := latestChild(ownedAttempts(t, e), p.ID, 5); a.State != "queued" {
		t.Fatal(a)
	}
	// The dependent child still waits for its prerequisite.
	if latestChild(ownedAttempts(t, e), p.ID, 4) != nil {
		t.Fatal("dependent child dispatched early")
	}
}

// Reworking a delivered child keeps its dependants waiting for the new delivery.
func TestSupervisorReworkOfDeliveredChild(t *testing.T) {
	e, g, p := supervisorSetup(t)
	ctx := context.Background()
	actionSuper(t, e, p, "start", false)
	e.dispatch(ctx)
	e.wg.Wait()
	first := ownedAttempts(t, e)[0]
	if first.State != "awaiting_review" {
		t.Fatal(first)
	}
	second, err := e.rework(ctx, p, g.nodes[3], "manual")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.Generation != 2 || second.SupervisorID != first.SupervisorID {
		t.Fatal(second)
	}
	e.poll(ctx)
	for _, a := range ownedAttempts(t, e) {
		if a.Issue.Number == 4 {
			t.Fatal("dependant started while its prerequisite was being reworked")
		}
	}
	e.dispatch(ctx)
	e.wg.Wait()
	if a, _ := e.current(second.ID); a.State != "awaiting_review" {
		t.Fatal(a)
	}
	g.close(3, "completed")
	e.poll(ctx)
	started := false
	for _, a := range ownedAttempts(t, e) {
		started = started || a.Issue.Number == 4
	}
	if !started {
		v, _ := e.Store.SupervisorFor(p.ID, 2)
		t.Fatal("dependant did not start after the reworked child closed", v.Reason)
	}
}

func TestClosureMatchesToleratesGitHubSkew(t *testing.T) {
	closed := time.Date(2026, 10, 8, 10, 34, 51, 0, time.UTC)
	for _, c := range []struct {
		event time.Time
		want  bool
	}{
		{closed, true},
		{closed.Add(time.Second), true},
		{closed.Add(-time.Second), true},
		{closed.Add(10 * time.Second), false},
		{time.Time{}, false},
	} {
		if got := closureMatches(c.event, closed); got != c.want {
			t.Errorf("closureMatches(%v) = %v, want %v", c.event, got, c.want)
		}
	}
}
