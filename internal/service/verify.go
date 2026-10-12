package service

import (
	"changathi/internal/core"
	"changathi/internal/provider"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// roleError carries the provider and model whose session failed, so an account
// limit gates only that provider (or model) instead of all dispatch.
type roleError struct {
	role core.Role
	err  error
}

func (r roleError) Error() string { return r.err.Error() }
func (r roleError) Unwrap() error { return r.err }

// errWorktreeChanged stops an attempt whose verifier modified the worktree.
var errWorktreeChanged = errors.New("verifier modified the worktree; inspect it before retrying")

const maxVerifyDiff = 200 * 1024

// provider resolves a role to a registered provider that the host permits.
func (e *Engine) provider(r core.Role) (provider.Provider, error) {
	if err := e.Config.Allows(r); err != nil {
		return nil, err
	}
	p := e.Providers[r.Provider]
	if p == nil {
		return nil, fmt.Errorf("provider %s is not available in this daemon", r.Provider)
	}
	return p, nil
}

// checkWorkflow applies host policy to every role before a workflow can dispatch.
func (e *Engine) checkWorkflow(flow core.Workflow) error {
	for name, r := range flow.Roles() {
		if _, err := e.provider(r); err != nil {
			return fmt.Errorf("workflow: agents.%s: %w", name, err)
		}
	}
	return nil
}

// gate returns the active account gate for a role, provider-wide or model-specific.
func (e *Engine) gate(r core.Role) *core.ProviderGate {
	for _, key := range []string{core.GateKey(r.Provider, ""), core.GateKey(r.Provider, r.Model)} {
		var g core.ProviderGate
		if e.Store.Get("gate", key, &g) == nil && g.Until.After(core.Now()) {
			return &g
		}
	}
	return nil
}

// setGate pauses a provider after an account limit. A limit message naming the
// role's model family (for example a weekly Opus window) gates only that model.
func (e *Engine) setGate(r core.Role, msg string, until time.Time) {
	model := ""
	low := strings.ToLower(msg)
	for _, family := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(strings.ToLower(r.Model), family) && strings.Contains(low, family) {
			model = r.Model
		}
	}
	_ = e.Store.Put("gate", core.GateKey(r.Provider, model), core.ProviderGate{Provider: r.Provider, Model: model, Until: until, Reason: msg})
}

// recordUsage aggregates per-model token and cost totals reported by Claude turns.
func (e *Engine) recordUsage(typ, text string) {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	usage := map[string]any{}
	_ = e.Store.Get("usage", "claude", &usage)
	switch typ {
	case "claude/usage":
		var v struct {
			Model string  `json:"model"`
			Cost  float64 `json:"cost_usd"`
			Usage struct {
				Input         float64 `json:"input_tokens"`
				Output        float64 `json:"output_tokens"`
				CacheRead     float64 `json:"cache_read_input_tokens"`
				CacheCreation float64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(text), &v) != nil || v.Model == "" {
			return
		}
		models, _ := usage["models"].(map[string]any)
		if models == nil {
			models = map[string]any{}
		}
		m, _ := models[v.Model].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		add := func(k string, n float64) { f, _ := m[k].(float64); m[k] = f + n }
		add("turns", 1)
		add("input_tokens", v.Usage.Input)
		add("output_tokens", v.Usage.Output)
		add("cache_read_input_tokens", v.Usage.CacheRead)
		add("cache_creation_input_tokens", v.Usage.CacheCreation)
		add("cost_usd", v.Cost)
		models[v.Model] = m
		usage["models"] = models
	case "claude/rate_limit":
		var v struct {
			Info json.RawMessage `json:"rate_limit_info"`
		}
		if json.Unmarshal([]byte(text), &v) != nil || len(v.Info) == 0 {
			return
		}
		var info any
		_ = json.Unmarshal(v.Info, &info)
		usage["rate_limit"] = info
	default:
		return
	}
	_ = e.Store.Put("usage", "claude", usage)
}

func verifierPrompt(p core.Project, a core.Attempt, r core.Role, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are an independent reviewer for GitHub issue #%d in %s. Another agent implemented it; you did not write this code.\n", a.Issue.Number, p.Repo)
	fmt.Fprintf(&b, "Title: %s\nDescription:\n%s\n\n", a.Issue.Title, a.Issue.Body)
	fmt.Fprintf(&b, "The change passed these validation commands: %v.\n", a.Workflow.Validation)
	b.WriteString("Decide whether the change satisfies the issue's acceptance criteria and is correct. Report only concrete problems: a `blocking` finding must be a defect, a missing requirement, or a missing test for changed behavior; anything else is a `note`. Do not edit files or commit; you are read-only. Inspect the repository with the read-only tools if the diff is not enough.\n")
	if r.Provider == "claude" {
		// Matches provider.Claude's verifier allow-list; anything else is denied without a prompt.
		b.WriteString("Your tools are Read, Grep and Glob, plus Bash for single `git diff`, `git log`, `git show` and `git status` commands only. Other commands, pipes and chained commands are denied. Changathi already ran the validation commands, so do not try to build or test; read files with Read rather than `cat`.\n")
	}
	if r.Instructions != "" {
		b.WriteString("\nProject review instructions:\n")
		b.WriteString(strings.TrimSpace(r.Instructions))
		b.WriteString("\n")
	}
	b.WriteString("\nReturn your verdict as the structured output when available. Otherwise end your reply with exactly one fenced block:\n```changathi-verdict\n{\"verdict\": \"pass\" | \"fail\", \"findings\": [{\"severity\": \"blocking\" | \"note\", \"file\": \"path\", \"line\": 1, \"summary\": \"...\"}]}\n```\n")
	b.WriteString("A fail verdict needs at least one blocking finding; a pass verdict has none.\n\nDiff under review:\n```diff\n")
	b.WriteString(diff)
	b.WriteString("\n```\n")
	return b.String()
}

// verifyOnce runs one independent verifier session on the validated head.
func (e *Engine) verifyOnce(ctx context.Context, p core.Project, a core.Attempt, r core.Role, head, base string) (core.Verification, error) {
	rec := core.Verification{Head: head, Provider: r.Provider, Model: r.Model, Created: core.Now()}
	prov, err := e.provider(r)
	if err != nil {
		return rec, err
	}
	diff, err := core.Command(ctx, a.Workspace, "git", "diff", base+"...HEAD")
	if err != nil {
		return rec, err
	}
	if len(diff) > maxVerifyDiff {
		diff = strings.ToValidUTF8(diff[:maxVerifyDiff], "") + fmt.Sprintf("\n[diff truncated; run git diff %s...HEAD for the rest]", base)
	}
	beforeHead, err := core.Command(ctx, a.Workspace, "git", "rev-parse", "HEAD")
	if err != nil {
		return rec, err
	}
	beforeStatus, err := core.Command(ctx, a.Workspace, "git", "status", "--porcelain")
	if err != nil {
		return rec, err
	}
	s, err := prov.Open(ctx)
	if err != nil {
		return rec, roleError{r, err}
	}
	defer s.Close()
	if err = s.Start(ctx, provider.Request{Workspace: a.Workspace, Model: r.Model, Effort: r.Effort, ReadOnly: true, Schema: core.VerdictSchema}); err != nil {
		return rec, roleError{r, err}
	}
	e.event(a.ID, "role", fmt.Sprintf("verify %s %s", r.Provider, r.Model))
	var final string
	var said []string
	err = s.Run(ctx, verifierPrompt(p, a, r, diff), func(v provider.Event) {
		if v.Thread != "" {
			rec.Session = v.Thread
		}
		if v.Type == "claude/result" {
			final = v.Text
		} else if v.Output != nil && v.Output.Kind == "assistant" && v.Output.Text != "" {
			said = append(said, v.Output.Text)
		}
		e.recordEvent(core.Event{Version: 1, ProjectID: p.ID, AttemptID: a.ID, ThreadID: rec.Session, TurnID: v.Turn, Type: v.Type, Text: v.Text, Output: v.Output})
	})
	_ = s.Close()
	// The worktree guard runs even after a failed turn: a verifier that wrote must not be trusted.
	afterHead, headErr := core.Command(ctx, a.Workspace, "git", "rev-parse", "HEAD")
	afterStatus, statusErr := core.Command(ctx, a.Workspace, "git", "status", "--porcelain")
	if headErr != nil || statusErr != nil || afterHead != beforeHead || afterStatus != beforeStatus {
		return rec, errWorktreeChanged
	}
	if err != nil {
		return rec, roleError{r, err}
	}
	verdict, parseErr := core.ParseVerdict(final)
	if parseErr != nil && final == "" {
		verdict, parseErr = core.ParseVerdict(strings.Join(said, "\n"))
	}
	if parseErr != nil {
		rec.Verdict, rec.Error = "malformed", parseErr.Error()
		return rec, nil
	}
	rec.Verdict, rec.Findings = verdict.Verdict, verdict.Findings
	return rec, nil
}

// verify runs the verifier, retrying a malformed verdict once on the same head.
// It returns the final record; a second malformed verdict is reported as such.
func (e *Engine) verify(ctx context.Context, p core.Project, a core.Attempt, r core.Role, head, base string) (core.Verification, error) {
	var rec core.Verification
	var err error
	for try := 0; try < 2; try++ {
		rec, err = e.verifyOnce(ctx, p, a, r, head, base)
		if err != nil {
			return rec, err
		}
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error {
			x.Verifications = append(x.Verifications, rec)
			if rec.Verdict == "fail" {
				x.VerifyFailures++
			}
			return nil
		})
		text := rec.Verdict
		if rec.Error != "" {
			text += ": " + rec.Error
		}
		if len(rec.Findings) > 0 {
			text += "\n" + core.FindingsText(rec.Findings, "blocking") + core.FindingsText(rec.Findings, "note")
		}
		e.event(a.ID, "verification", text)
		if rec.Verdict != "malformed" {
			return rec, nil
		}
	}
	return rec, nil
}

// postVerificationNotes posts a passing verdict's notes as a PR review comment,
// once per verified head; the operation journal prevents duplicates after restart.
func (e *Engine) postVerificationNotes(ctx context.Context, p core.Project, a core.Attempt, pr *core.PR) {
	a, err := e.current(a.ID)
	if err != nil {
		return
	}
	v := latestVerification(a, pr.Head)
	if v == nil || v.Verdict != "pass" {
		return
	}
	notes := core.FindingsText(v.Findings, "note")
	if notes == "" {
		return
	}
	kind := "verify_comment:" + pr.Head
	ops, _ := core.List[core.Operation](e.Store, "operation")
	for _, op := range ops {
		if op.AttemptID == a.ID && op.Kind == kind && op.State != "uncertain" {
			return
		}
	}
	body := fmt.Sprintf("**Changathi verification** (%s `%s`) passed for %s with notes:\n\n%s", v.Provider, v.Model, shortSHA(pr.Head), notes)
	if err = e.operation(a.ID, kind, func() error { return e.GitHub.ReviewComment(ctx, p, pr.Number, body) }); err != nil {
		e.event(a.ID, "verification", "posting review comment failed: "+err.Error())
	}
}

func latestVerification(a core.Attempt, head string) *core.Verification {
	for i := len(a.Verifications) - 1; i >= 0; i-- {
		if a.Verifications[i].Head == head {
			return &a.Verifications[i]
		}
	}
	return nil
}
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
