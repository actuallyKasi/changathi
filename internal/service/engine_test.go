package service

import (
	"changathi/internal/core"
	"changathi/internal/provider"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeGH struct {
	mu       sync.Mutex
	issues   map[string]core.Issue
	prs      map[string]core.PR
	creates  int
	merges   int
	comments []string
}

func (g *fakeGH) Repository(context.Context, string) (core.Project, error) {
	return core.Project{}, nil
}
func (g *fakeGH) Issue(_ context.Context, p core.Project, _ int) (core.Issue, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.issues[p.ID], nil
}
func (g *fakeGH) Issues(_ context.Context, p core.Project, ready bool) ([]core.Issue, error) {
	i, _ := g.Issue(context.Background(), p, 1)
	if i.State == "open" && (!ready || i.Ready()) {
		return []core.Issue{i}, nil
	}
	return nil, nil
}
func (g *fakeGH) FindPR(_ context.Context, p core.Project, branch string) (*core.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pr, ok := g.prs[branch]
	if !ok {
		return nil, nil
	}
	return &pr, nil
}
func (g *fakeGH) CreatePR(_ context.Context, p core.Project, a core.Attempt) (*core.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.creates++
	pr := core.PR{Number: g.creates, State: "OPEN", Head: a.Head, Branch: a.Branch, Base: p.Base, BaseSHA: a.BaseSHA, MergeState: "CLEAN", Mergeable: "MERGEABLE"}
	g.prs[a.Branch] = pr
	return &pr, nil
}
func (g *fakeGH) ReviewComment(_ context.Context, _ core.Project, number int, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.comments = append(g.comments, body)
	return nil
}
func (g *fakeGH) Merge(_ context.Context, p core.Project, pr *core.PR, _ string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	actual := g.prs[pr.Branch]
	if actual.Head != pr.Head {
		return fmt.Errorf("head changed")
	}
	g.merges++
	actual.State = "MERGED"
	g.prs[pr.Branch] = actual
	return nil
}
func (g *fakeGH) MarkReview(_ context.Context, p core.Project, _ int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := g.issues[p.ID]
	i.Labels = []core.Label{{Name: "agent:review"}}
	g.issues[p.ID] = i
	return nil
}
func (g *fakeGH) Prepare(context.Context, core.Project) ([]string, error) {
	return nil, nil
}

type fakeProvider struct {
	block   <-chan struct{}
	entered chan string
}
type fakeSession struct {
	workspace string
	p         *fakeProvider
}

func (p *fakeProvider) Open(context.Context) (provider.Session, error) {
	return &fakeSession{p: p}, nil
}
func (s *fakeSession) Start(_ context.Context, r provider.Request) error {
	s.workspace = r.Workspace
	return nil
}
func (s *fakeSession) Run(ctx context.Context, _ string, emit func(provider.Event)) error {
	emit(provider.Event{Type: "session_started", Thread: "fake-thread", Turn: "fake-turn"})
	if s.p.entered != nil {
		s.p.entered <- s.workspace
	}
	if s.p.block != nil {
		select {
		case <-s.p.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if e := os.WriteFile(filepath.Join(s.workspace, "feature.txt"), []byte("feature\n"), 0600); e != nil {
		return e
	}
	for _, args := range [][]string{{"add", "feature.txt"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "Implement feature"}} {
		if _, e := core.Command(ctx, s.workspace, "git", args...); e != nil {
			return e
		}
	}
	return nil
}
func (s *fakeSession) Close() error { return nil }
func setup(t *testing.T, n int, prov *fakeProvider) (*Engine, *fakeGH, []core.Project) {
	t.Helper()
	dir := t.TempDir()
	store, err := core.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	gh := &fakeGH{issues: map[string]core.Issue{}, prs: map[string]core.PR{}}
	cfg := core.DefaultConfig()
	cfg.DataDir = dir
	cfg.PollSeconds = 1
	e := New(cfg, store, gh, map[string]provider.Provider{"codex": prov})
	ps := []core.Project{}
	ctx := context.Background()
	for j := 0; j < n; j++ {
		p := core.Project{ID: core.ID(), Name: fmt.Sprintf("p%d", j), RepoID: int64(j + 1), Repo: "fake/repo", Base: "main", Enabled: true, Auto: true, Concurrency: 1}
		remote := filepath.Join(dir, fmt.Sprintf("remote%d", j))
		_, err = core.Command(ctx, "", "git", "init", "-b", "main", remote)
		if err != nil {
			t.Fatal(err)
		}
		workflow := "---\nvalidation:\n  - test -f feature.txt\n---\nImplement the issue."
		_ = os.WriteFile(filepath.Join(remote, "WORKFLOW.md"), []byte(workflow), 0600)
		_, _ = core.Command(ctx, remote, "git", "add", ".")
		_, err = core.Command(ctx, remote, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "Initial")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = core.Command(ctx, remote, "git", "config", "receive.denyCurrentBranch", "ignore")
		clone := e.Workspaces.Clone(p)
		_ = os.MkdirAll(filepath.Dir(clone), 0700)
		_, err = core.Command(ctx, "", "git", "clone", "--no-checkout", remote, clone)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.AddProject(p); err != nil {
			t.Fatal(err)
		}
		gh.issues[p.ID] = core.Issue{ID: int64(j + 1), Number: 1, Title: "feature", State: "open", Labels: []core.Label{{Name: "agent:ready"}}}
		ps = append(ps, p)
	}
	return e, gh, ps
}
func TestEndToEndTwoRepositories(t *testing.T) {
	e, gh, ps := setup(t, 2, &fakeProvider{})
	ctx := context.Background()
	var wg sync.WaitGroup
	ids := []string{}
	for _, p := range ps {
		a, err := e.Store.Enqueue(p, gh.issues[p.ID], "auto", false)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
		wg.Add(1)
		go func(p core.Project, a core.Attempt) { defer wg.Done(); e.execute(ctx, p, a) }(p, a)
	}
	wg.Wait()
	for j, id := range ids {
		a, _ := e.current(id)
		if a.State != "awaiting_review" {
			t.Fatalf("state=%s error=%s", a.State, a.Error)
		}
		i, _ := gh.Issue(ctx, ps[j], 1)
		if i.State != "open" || i.Ready() {
			t.Fatal("review issue lifecycle incorrect")
		}
		same, _ := e.Store.Enqueue(ps[j], i, "auto", false)
		if same.ID != id {
			t.Fatal("review issue redispatched")
		}
	}
	if gh.creates != 2 || gh.merges != 2 {
		t.Fatal("delivery count")
	}
	a, _ := e.current(ids[0])
	e.state(a.ID, "retry_wait", "simulated crash after merge")
	e.execute(ctx, ps[0], a)
	if gh.creates != 2 || gh.merges != 2 {
		t.Fatal("recovery duplicated delivery")
	}
	gh.mu.Lock()
	i := gh.issues[ps[0].ID]
	i.State = "closed"
	gh.issues[ps[0].ID] = i
	gh.mu.Unlock()
	e.poll(ctx)
	a, _ = e.current(ids[0])
	if a.State != "done" {
		t.Fatal("human closure did not finish issue")
	}
}
func TestSchedulerCancellation(t *testing.T) {
	blocked := make(chan struct{})
	entered := make(chan string, 5)
	e, _, ps := setup(t, 2, &fakeProvider{block: blocked, entered: entered})
	e.Config.Concurrency = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, p := range ps {
		_, _ = e.Store.Enqueue(p, core.Issue{ID: p.RepoID, Number: 1, State: "open"}, "manual", false)
	}
	e.dispatch(ctx)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker not started")
	}
	e.mu.Lock()
	if len(e.workers) != 1 {
		t.Fatal("global capacity exceeded")
	}
	var id string
	for k := range e.workers {
		id = k
	}
	e.mu.Unlock()
	if err := e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	e.wg.Wait()
	a, _ := e.current(id)
	if a.State != "cancelled" {
		t.Fatal("cancel state overwritten")
	}
	e.dispatch(ctx)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("next project starved")
	}
	cancel()
	e.wg.Wait()
}
func TestMergeGates(t *testing.T) {
	p := core.Project{Base: "main"}
	a := core.Attempt{Branch: "branch", Head: "head", BaseSHA: "base", Workflow: core.Workflow{RequiredChecks: []string{"test"}}}
	good := core.PR{State: "OPEN", Branch: "branch", Head: "head", Base: "main", BaseSHA: "base", Mergeable: "MERGEABLE", MergeState: "CLEAN", Checks: []core.Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}}
	tests := []struct {
		name   string
		modify func(*core.PR)
		want   string
		bad    bool
	}{{"valid", func(*core.PR) {}, "merge", false}, {"stale head", func(p *core.PR) { p.Head = "other" }, "revalidate", false}, {"stale base", func(p *core.PR) { p.BaseSHA = "other" }, "revalidate", false}, {"pending", func(p *core.PR) { p.Checks[0].Status = "IN_PROGRESS"; p.Checks[0].Conclusion = "" }, "wait", false}, {"missing", func(p *core.PR) { p.Checks = nil }, "wait", false}, {"failed", func(p *core.PR) { p.Checks[0].Conclusion = "FAILURE" }, "", true}, {"protected", func(p *core.PR) { p.MergeState = "BLOCKED" }, "", true}, {"closing", func(p *core.PR) { p.Body = "Fixes #1" }, "", true}, {"wrong branch", func(p *core.PR) { p.Branch = "foreign" }, "", true}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := good
			pr.Checks = append([]core.Check(nil), good.Checks...)
			tt.modify(&pr)
			decision, err := CheckPR(p, a, &pr)
			if (err != nil) != tt.bad || decision != tt.want {
				t.Fatalf("%q %v", decision, err)
			}
		})
	}
}

func TestAutomaticEligibilityAndReactivation(t *testing.T) {
	blocked := make(chan struct{})
	entered := make(chan string, 2)
	e, gh, ps := setup(t, 1, &fakeProvider{block: blocked, entered: entered})
	p := ps[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, _ := e.Store.Enqueue(p, gh.issues[p.ID], "auto", false)
	e.dispatch(ctx)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker not started")
	}
	gh.mu.Lock()
	i := gh.issues[p.ID]
	i.Labels = nil
	gh.issues[p.ID] = i
	gh.mu.Unlock()
	e.poll(ctx)
	e.wg.Wait()
	a, _ = e.current(a.ID)
	if a.State != "cancelled" || !a.ReadyRemoved {
		t.Fatalf("not stopped: %+v", a)
	}
	gh.mu.Lock()
	i.Labels = []core.Label{{Name: "agent:ready"}}
	gh.issues[p.ID] = i
	gh.mu.Unlock()
	e.poll(ctx)
	a, _ = e.current(a.ID)
	if a.State != "queued" || a.ReadyRemoved {
		t.Fatal("readiness reactivation failed")
	}
	_ = e.Cancel(a.ID)
}

func TestWorkflowRepairUnblocksProject(t *testing.T) {
	e, gh, ps := setup(t, 1, &fakeProvider{})
	p := ps[0]
	remote := filepath.Join(e.Config.DataDir, "remote0")
	ctx := context.Background()
	_ = os.WriteFile(filepath.Join(remote, "WORKFLOW.md"), []byte("invalid workflow"), 0600)
	_, _ = core.Command(ctx, remote, "git", "add", ".")
	_, err := core.Command(ctx, remote, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "Bad workflow")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := e.Store.Enqueue(p, gh.issues[p.ID], "auto", false)
	e.execute(ctx, p, a)
	a, _ = e.current(a.ID)
	stored, _ := e.Store.Project(p.ID)
	if a.State != "needs_attention" || stored.Error == "" {
		t.Fatal("configuration did not block project")
	}
	_ = os.WriteFile(filepath.Join(remote, "WORKFLOW.md"), []byte("---\nvalidation: [test -f feature.txt]\n---\nImplement"), 0600)
	_, _ = core.Command(ctx, remote, "git", "add", ".")
	_, err = core.Command(ctx, remote, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "Repair workflow")
	if err != nil {
		t.Fatal(err)
	}
	e.poll(ctx)
	a, _ = e.current(a.ID)
	stored, _ = e.Store.Project(p.ID)
	if a.State != "queued" || stored.Error != "" {
		t.Fatalf("workflow repair failed %s %s", a.State, stored.Error)
	}
}

func TestProviderLimitPausesAllDispatch(t *testing.T) {
	e, gh, ps := setup(t, 2, &fakeProvider{})
	a, _ := e.Store.Enqueue(ps[0], gh.issues[ps[0].ID], "manual", false)
	_, _ = e.Store.Enqueue(ps[1], gh.issues[ps[1].ID], "manual", false)
	e.fail(context.Background(), a.ID, fmt.Errorf("usage limit reached"))
	e.dispatch(context.Background())
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.workers) != 0 {
		t.Fatal("account limit did not pause dispatch across projects")
	}
	a, _ = e.current(a.ID)
	if a.State != "waiting_account" || a.Due.IsZero() {
		t.Fatal("work not preserved in account wait")
	}
}
