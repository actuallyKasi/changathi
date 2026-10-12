package service

import (
	"changathi/internal/core"
	"changathi/internal/provider"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// turnProvider commits a fresh change every turn and records each prompt, so a
// verification failure can be traced into the next implement turn.
type turnProvider struct {
	mu       sync.Mutex
	prompts  []string
	requests []provider.Request
	lose     bool // a resumed thread reports ErrSessionLost
}
type turnSession struct {
	p         *turnProvider
	workspace string
	thread    string
}

func (p *turnProvider) Open(context.Context) (provider.Session, error) {
	return &turnSession{p: p}, nil
}
func (s *turnSession) Start(_ context.Context, r provider.Request) error {
	s.workspace, s.thread = r.Workspace, r.Thread
	s.p.mu.Lock()
	s.p.requests = append(s.p.requests, r)
	s.p.mu.Unlock()
	return nil
}
func (s *turnSession) Run(ctx context.Context, prompt string, emit func(provider.Event)) error {
	s.p.mu.Lock()
	s.p.prompts = append(s.p.prompts, prompt)
	n := len(s.p.prompts)
	lose := s.p.lose && s.thread != ""
	s.p.mu.Unlock()
	if lose {
		return fmt.Errorf("%w: %s", provider.ErrSessionLost, s.thread)
	}
	emit(provider.Event{Type: "session_started", Thread: "impl-thread", Turn: fmt.Sprint(n)})
	if e := os.WriteFile(filepath.Join(s.workspace, "feature.txt"), []byte(fmt.Sprintf("feature %d\n", n)), 0600); e != nil {
		return e
	}
	for _, args := range [][]string{{"add", "feature.txt"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", fmt.Sprintf("Implement feature %d", n)}} {
		if _, e := core.Command(ctx, s.workspace, "git", args...); e != nil {
			return e
		}
	}
	return nil
}
func (s *turnSession) Close() error { return nil }

// verifierProvider replays scripted verdicts, optionally misbehaving by writing.
type verifierProvider struct {
	mu       sync.Mutex
	verdicts []string
	runs     int
	write    bool
	errs     []error // returned by successive runs before any verdict
	requests []provider.Request
	prompts  []string
}
type verifierSession struct {
	p         *verifierProvider
	workspace string
}

func (p *verifierProvider) Open(context.Context) (provider.Session, error) {
	return &verifierSession{p: p}, nil
}
func (s *verifierSession) Start(_ context.Context, r provider.Request) error {
	s.workspace = r.Workspace
	s.p.mu.Lock()
	s.p.requests = append(s.p.requests, r)
	s.p.mu.Unlock()
	return nil
}
func (s *verifierSession) Run(_ context.Context, prompt string, emit func(provider.Event)) error {
	s.p.mu.Lock()
	if len(s.p.errs) > 0 {
		err := s.p.errs[0]
		s.p.errs = s.p.errs[1:]
		s.p.mu.Unlock()
		return err
	}
	verdict := s.p.verdicts[min(s.p.runs, len(s.p.verdicts)-1)]
	s.p.runs++
	s.p.prompts = append(s.p.prompts, prompt)
	s.p.mu.Unlock()
	emit(provider.Event{Type: "session_started", Thread: fmt.Sprintf("verify-%d", s.p.runs)})
	if s.p.write {
		_ = os.WriteFile(filepath.Join(s.workspace, "sneaky.txt"), []byte("x"), 0600)
	}
	emit(provider.Event{Type: "claude/result", Text: verdict})
	return nil
}
func (s *verifierSession) Close() error { return nil }

const (
	passWithNote = `{"verdict":"pass","findings":[{"severity":"note","file":"feature.txt","line":1,"summary":"Consider a trailing newline check."}]}`
	failBlocking = `{"verdict":"fail","findings":[{"severity":"blocking","file":"feature.txt","line":1,"summary":"Missing test for the feature."}]}`
)

const verifyWorkflow = `---
agents:
  implement:
    provider: codex
  verify:
    provider: claude
    model: claude-sonnet-5-5
    effort: low
    instructions: Block on missing tests.
validation:
  - test -f feature.txt
max_turns: 5
max_verify_rounds: 2
---
Implement the issue.`

// setupVerify registers an implementer and a Claude verifier, and commits a
// verifying workflow to the project's remote.
func setupVerify(t *testing.T, workflow string, verdicts ...string) (*Engine, *fakeGH, core.Project, *turnProvider, *verifierProvider) {
	t.Helper()
	e, gh, ps := setup(t, 1, &fakeProvider{})
	impl, verifier := &turnProvider{}, &verifierProvider{verdicts: verdicts}
	e.Providers = map[string]provider.Provider{"codex": impl, "claude": verifier}
	e.Config.Providers = map[string]core.ProviderPolicy{"codex": {}, "claude": {Models: []string{"claude-opus-5-5", "claude-sonnet-5-5"}}}
	remote := filepath.Join(e.Config.DataDir, "remote0")
	if err := os.WriteFile(filepath.Join(remote, "WORKFLOW.md"), []byte(workflow), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Command(context.Background(), remote, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-am", "Verify workflow"); err != nil {
		t.Fatal(err)
	}
	return e, gh, ps[0], impl, verifier
}

func run(t *testing.T, e *Engine, gh *fakeGH, p core.Project) core.Attempt {
	t.Helper()
	a, err := e.Store.Enqueue(p, gh.issues[p.ID], "auto", false)
	if err != nil {
		t.Fatal(err)
	}
	e.execute(context.Background(), p, a)
	a, _ = e.current(a.ID)
	return a
}

func TestVerificationPassPublishesWithNotes(t *testing.T) {
	e, gh, p, impl, verifier := setupVerify(t, verifyWorkflow, passWithNote)
	a := run(t, e, gh, p)
	if a.State != "awaiting_review" {
		t.Fatalf("state=%s error=%s", a.State, a.Error)
	}
	if len(verifier.requests) != 1 || !verifier.requests[0].ReadOnly || verifier.requests[0].Schema == "" || verifier.requests[0].Model != "claude-sonnet-5-5" || verifier.requests[0].Effort != "low" {
		t.Fatalf("verifier request %+v", verifier.requests)
	}
	if impl.requests[0].ReadOnly {
		t.Fatal("implementer started read-only")
	}
	if !strings.Contains(verifier.prompts[0], "Block on missing tests.") || !strings.Contains(verifier.prompts[0], "+feature 1") {
		t.Fatal("verifier prompt lacks instructions or diff")
	}
	if len(a.Verifications) != 1 || a.Verifications[0].Verdict != "pass" || a.Verifications[0].Head != a.Head {
		t.Fatalf("verifications %+v head %s", a.Verifications, a.Head)
	}
	if len(gh.comments) != 1 || !strings.Contains(gh.comments[0], "feature.txt:1: Consider a trailing newline check.") {
		t.Fatalf("review comments %q", gh.comments)
	}
	// Recovery after publication must not post the same notes twice.
	e.postVerificationNotes(context.Background(), p, a, &core.PR{Number: 1, Head: a.Head})
	if len(gh.comments) != 1 {
		t.Fatal("duplicate review comment")
	}
}

func TestVerificationFailureFeedsNextTurnThenPasses(t *testing.T) {
	e, gh, p, impl, _ := setupVerify(t, verifyWorkflow, failBlocking, passWithNote)
	a := run(t, e, gh, p)
	if a.State != "awaiting_review" {
		t.Fatalf("state=%s error=%s", a.State, a.Error)
	}
	if len(impl.prompts) != 2 || !strings.Contains(impl.prompts[1], "Missing test for the feature.") {
		t.Fatalf("fix-up prompt missing findings: %q", impl.prompts)
	}
	if a.Turns != 2 || a.VerifyFailures != 1 || len(a.Verifications) != 2 {
		t.Fatalf("turns=%d failures=%d verifications=%d", a.Turns, a.VerifyFailures, len(a.Verifications))
	}
}

func TestVerificationRoundLimitBlocksPublication(t *testing.T) {
	e, gh, p, _, _ := setupVerify(t, verifyWorkflow, failBlocking)
	a := run(t, e, gh, p)
	if a.State != "needs_attention" || !strings.Contains(a.Error, "verification failed 2 times") {
		t.Fatalf("state=%s error=%s", a.State, a.Error)
	}
	if gh.creates != 0 {
		t.Fatal("PR opened for an unverified head")
	}
	// An explicit retry restores the verification rounds.
	retried, err := e.Store.Enqueue(p, gh.issues[p.ID], "manual", true)
	if err != nil || retried.VerifyFailures != 0 || retried.ID != a.ID {
		t.Fatalf("retry %+v %v", retried, err)
	}
}

func TestMalformedVerdictIsNeverAPass(t *testing.T) {
	e, gh, p, _, verifier := setupVerify(t, verifyWorkflow, "looks good to me")
	a := run(t, e, gh, p)
	if a.State != "needs_attention" || verifier.runs != 2 || gh.creates != 0 {
		t.Fatalf("state=%s runs=%d creates=%d", a.State, verifier.runs, gh.creates)
	}
}

func TestVerifierThatWritesIsStopped(t *testing.T) {
	e, gh, p, _, verifier := setupVerify(t, verifyWorkflow, passWithNote)
	verifier.write = true
	a := run(t, e, gh, p)
	if a.State != "needs_attention" || !strings.Contains(a.Error, "verifier modified the worktree") || gh.creates != 0 {
		t.Fatalf("state=%s error=%s creates=%d", a.State, a.Error, gh.creates)
	}
	if _, err := os.Stat(filepath.Join(a.Workspace, "sneaky.txt")); err != nil {
		t.Fatal("worktree was not preserved for inspection")
	}
}

func TestHostAllowListBlocksWorkflow(t *testing.T) {
	workflow := strings.Replace(verifyWorkflow, "claude-sonnet-5-5", "claude-fable-5-1", 1)
	e, gh, p, _, _ := setupVerify(t, workflow, passWithNote)
	if _, err := e.Store.Enqueue(p, gh.issues[p.ID], "auto", false); err != nil {
		t.Fatal(err)
	}
	e.dispatch(context.Background())
	latest, _ := e.Store.Project(p.ID)
	if !strings.Contains(latest.Error, `model "claude-fable-5-1" is not allowed`) {
		t.Fatalf("project error %q", latest.Error)
	}
}

func TestClaudeLimitGatesOnlyThatModel(t *testing.T) {
	e, _, _, _, _ := setupVerify(t, verifyWorkflow, passWithNote)
	opus := core.Role{Provider: "claude", Model: "claude-opus-5-5"}
	e.fail(context.Background(), "missing", roleError{opus, fmt.Errorf("Claude usage limit (seven_day_opus): weekly Opus limit reached")})
	if e.gate(opus) == nil {
		t.Fatal("Opus not gated")
	}
	if e.gate(core.Role{Provider: "claude", Model: "claude-sonnet-5-5"}) != nil || e.gate(core.Role{Provider: "codex"}) != nil {
		t.Fatal("limit leaked to another model or provider")
	}
}

func TestThreadNotResumedAcrossProviders(t *testing.T) {
	workflow := strings.Replace(verifyWorkflow, "  implement:\n    provider: codex\n", "  implement:\n    provider: claude\n    model: claude-opus-5-5\n", 1)
	e, gh, p, impl, _ := setupVerify(t, workflow, passWithNote)
	e.Providers["claude"] = &routedProvider{implement: impl, verify: e.Providers["claude"]}
	a, err := e.Store.Enqueue(p, gh.issues[p.ID], "auto", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Thread = "codex-thread"; return nil })
	a, _ = e.current(a.ID)
	e.execute(context.Background(), p, a)
	if len(impl.requests) == 0 || impl.requests[0].Thread != "" || impl.requests[0].Model != "claude-opus-5-5" {
		t.Fatalf("implement request %+v", impl.requests)
	}
	a, _ = e.current(a.ID)
	if a.ThreadProvider != "claude" {
		t.Fatalf("thread provider %q", a.ThreadProvider)
	}
}

// routedProvider serves both roles of one provider name from separate fakes.
type routedProvider struct{ implement, verify provider.Provider }
type routedSession struct {
	r *routedProvider
	provider.Session
}

func (r *routedProvider) Open(context.Context) (provider.Session, error) {
	return &routedSession{r: r}, nil
}
func (s *routedSession) Start(ctx context.Context, req provider.Request) error {
	target := s.r.implement
	if req.ReadOnly {
		target = s.r.verify
	}
	session, err := target.Open(ctx)
	if err != nil {
		return err
	}
	s.Session = session
	return session.Start(ctx, req)
}

// Opt-in: Opus implements and Sonnet verifies a tiny issue through the real CLI,
// with GitHub faked. Spends a few small turns of quota.
func TestLiveClaudeImplementAndVerify(t *testing.T) {
	if os.Getenv("CHANGATHI_CLAUDE_SMOKE") != "1" {
		t.Skip("set CHANGATHI_CLAUDE_SMOKE=1 to run live Claude roles")
	}
	workflow := `---
agents:
  implement:
    provider: claude
    model: claude-opus-5-5
    effort: low
  verify:
    provider: claude
    model: claude-sonnet-5-5
    effort: low
validation:
  - grep -qx hello feature.txt
---
Keep the change minimal.`
	e, gh, p, _, _ := setupVerify(t, workflow)
	e.Providers = map[string]provider.Provider{"claude": provider.Claude{Command: "claude"}}
	gh.issues[p.ID] = core.Issue{ID: 1, Number: 1, Title: "Add feature.txt", Body: "Create feature.txt containing exactly one line: hello. Commit it with git.", State: "open", Labels: []core.Label{{Name: "agent:ready"}}}
	a := run(t, e, gh, p)
	t.Logf("state=%s turns=%d verifications=%+v comments=%q", a.State, a.Turns, a.Verifications, gh.comments)
	if a.State != "awaiting_review" {
		t.Fatalf("state=%s error=%s", a.State, a.Error)
	}
	if a.ThreadProvider != "claude" || len(a.Verifications) == 0 || a.Verifications[len(a.Verifications)-1].Model != "claude-sonnet-5-5" {
		t.Fatalf("roles not recorded: %+v", a)
	}
	usage := map[string]any{}
	_ = e.Store.Get("usage", "claude", &usage)
	models, _ := usage["models"].(map[string]any)
	if models["claude-opus-5-5"] == nil || models["claude-sonnet-5-5"] == nil {
		t.Fatalf("usage not aggregated per model: %v", usage)
	}
}

func TestResumeAtVerificationSkipsImplementTurn(t *testing.T) {
	e, gh, p, impl, verifier := setupVerify(t, verifyWorkflow, passWithNote)
	verifier.errs = []error{fmt.Errorf("connection reset by peer")}
	a := run(t, e, gh, p)
	if a.State != "retry_wait" || a.NextStep != "validate" || len(impl.prompts) != 1 {
		t.Fatalf("after interrupted verification: state=%s next=%q implement turns=%d", a.State, a.NextStep, len(impl.prompts))
	}
	e.execute(context.Background(), p, a)
	a, _ = e.current(a.ID)
	if a.State != "awaiting_review" {
		t.Fatalf("state=%s error=%s", a.State, a.Error)
	}
	if len(impl.prompts) != 1 || len(impl.requests) != 1 || a.Turns != 1 {
		t.Fatalf("resume ran the implementer: prompts=%d sessions=%d turns=%d", len(impl.prompts), len(impl.requests), a.Turns)
	}
}

func TestVerifierGateWaitResumesAtVerification(t *testing.T) {
	e, gh, p, impl, _ := setupVerify(t, verifyWorkflow, passWithNote)
	sonnet := core.Role{Provider: "claude", Model: "claude-sonnet-5-5"}
	e.setGate(sonnet, "Claude usage limit", core.Now().Add(time.Hour))
	a := run(t, e, gh, p)
	if a.State != "waiting_account" || a.NextStep != "validate" {
		t.Fatalf("state=%s next=%q", a.State, a.NextStep)
	}
	e.setGate(sonnet, "limit reset", core.Now().Add(-time.Second))
	e.execute(context.Background(), p, a)
	a, _ = e.current(a.ID)
	if a.State != "awaiting_review" || len(impl.prompts) != 1 {
		t.Fatalf("state=%s implement turns=%d", a.State, len(impl.prompts))
	}
}

func TestLostSessionContinuesInFreshSession(t *testing.T) {
	e, gh, p, impl, _ := setupVerify(t, verifyWorkflow, passWithNote)
	impl.lose = true
	a, err := e.Store.Enqueue(p, gh.issues[p.ID], "auto", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error {
		x.Thread, x.ThreadProvider, x.LastFailure = "gone-thread", "codex", "check build failed"
		return nil
	})
	a, _ = e.current(a.ID)
	e.execute(context.Background(), p, a)
	a, _ = e.current(a.ID)
	if a.State != "awaiting_review" {
		t.Fatalf("state=%s error=%s", a.State, a.Error)
	}
	if len(impl.requests) != 2 || impl.requests[0].Thread != "gone-thread" || impl.requests[1].Thread != "" {
		t.Fatalf("sessions %+v", impl.requests)
	}
	recovery := impl.prompts[1]
	for _, want := range []string{"could not be resumed", "Commits on the branch", "check build failed", "Work on GitHub issue #1"} {
		if !strings.Contains(recovery, want) {
			t.Fatalf("recovery prompt lacks %q:\n%s", want, recovery)
		}
	}
	if a.Turns != 1 {
		t.Fatalf("recovery counted as an extra turn: %d", a.Turns)
	}
}

func TestPromptUsesLastFailureAndPreviousGeneration(t *testing.T) {
	p := core.Project{Repo: "o/r"}
	flow := core.Workflow{Prompt: "Do it.", Validation: []string{"true"}}
	a := core.Attempt{Issue: core.Issue{Number: 7, Title: "t"}, Error: "recovering interrupted operation", LastFailure: "Validation failed: TestX"}
	prompt, err := implementPrompt(p, a, flow)
	if err != nil || !strings.Contains(prompt, "Validation failed: TestX") || strings.Contains(prompt, "recovering interrupted operation") {
		t.Fatalf("prompt %q %v", prompt, err)
	}
	a = core.Attempt{Issue: core.Issue{Number: 7}, Previous: &core.PreviousGeneration{Generation: 1, PR: 12, State: "awaiting_review"}}
	if prompt, _ = implementPrompt(p, a, flow); !strings.Contains(prompt, "PR #12 was merged") {
		t.Fatalf("rework prompt %q", prompt)
	}
	a.Previous = &core.PreviousGeneration{Generation: 1, PR: 12, State: "needs_attention", LastFailure: "managed PR was closed without merging"}
	if prompt, _ = implementPrompt(p, a, flow); !strings.Contains(prompt, "was not merged") || !strings.Contains(prompt, "closed without merging") {
		t.Fatalf("unmerged rework prompt %q", prompt)
	}
}

func TestReworkGenerationCarriesPrevious(t *testing.T) {
	e, gh, p, _, _ := setupVerify(t, verifyWorkflow, passWithNote)
	first := run(t, e, gh, p)
	if first.State != "awaiting_review" || first.PR == 0 {
		t.Fatalf("state=%s pr=%d", first.State, first.PR)
	}
	rework, err := e.Store.Enqueue(p, gh.issues[p.ID], "manual", true)
	if err != nil || rework.ID == first.ID || rework.Previous == nil || rework.Previous.PR != first.PR || rework.Previous.Generation != first.Generation {
		t.Fatalf("rework %+v %v", rework.Previous, err)
	}
}

// A Claude verifier is told its read-only tool surface, so it does not spend
// calls on commands that dontAsk mode denies.
func TestVerifierPromptNamesClaudeTools(t *testing.T) {
	a := core.Attempt{Issue: core.Issue{Number: 2, Title: "t"}}
	p := core.Project{Repo: "o/r"}
	claude := verifierPrompt(p, a, core.Role{Provider: "claude", Model: "claude-sonnet-5-5"}, "diff")
	if !strings.Contains(claude, "`git diff`, `git log`, `git show` and `git status` commands only") || !strings.Contains(claude, "do not try to build or test") {
		t.Fatalf("claude verifier prompt:\n%s", claude)
	}
	if codex := verifierPrompt(p, a, core.Role{Provider: "codex"}, "diff"); strings.Contains(codex, "git status` commands only") {
		t.Fatal("codex verifier told about Claude's allow-list")
	}
}
