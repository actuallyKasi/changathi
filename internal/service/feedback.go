package service

import (
	"changathi/internal/core"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The review loop runs on GitHub: Changathi posts why an attempt stopped or what
// it delivered, the operator answers with issue comments and the agent:rework
// label, and rework prompts carry those comments.

// rework re-queues an issue on the operator's explicit request (`issue retry` or
// the agent:rework label): the same generation when nothing merged, a new one
// with a new branch and PR after merge. The operator's issue comments since the
// last capture are stored on the attempt for its prompt.
func (e *Engine) rework(ctx context.Context, p core.Project, i core.Issue, trigger string) (core.Attempt, error) {
	as, err := core.List[core.Attempt](e.Store, "attempt")
	if err != nil {
		return core.Attempt{}, err
	}
	prev := latestAttempt(as, p.ID, i.ID)
	g, hasFeedback := e.GitHub.(core.Feedback)
	if prev == nil || prev.Active() || !hasFeedback {
		return e.enqueueIssue(ctx, p, i, true, trigger)
	}
	since := prev.FeedbackAt
	if since.IsZero() {
		since = prev.Created
	}
	operator, err := g.Operator(ctx)
	if err != nil {
		return core.Attempt{}, err
	}
	comments, err := g.EvidenceComments(ctx, p, i.Number)
	if err != nil {
		return core.Attempt{}, fmt.Errorf("read operator comments: %w", err)
	}
	feedback, newest := feedbackText(comments, operator, since)
	a, err := e.enqueueIssue(ctx, p, i, true, trigger)
	if err != nil {
		return a, err
	}
	a.Feedback, a.FeedbackAt = feedback, newest
	_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error { x.Feedback, x.FeedbackAt = feedback, newest; return nil })
	text := fmt.Sprintf("rework requested (%s) for generation %d", trigger, a.Generation)
	if feedback != "" {
		text += " with operator comments"
	}
	e.event(a.ID, "rework", text)
	if a.ID != prev.ID {
		// A new generation is no longer awaiting review.
		if err := g.RemoveLabel(ctx, p, i.Number, "agent:review"); err != nil {
			e.event(a.ID, "rework", "removing agent:review failed: "+err.Error())
		}
	}
	return a, nil
}

// pollRework honours agent:rework on a project's open issues. A request made
// while the issue's attempt is still working takes effect once it stops.
func (e *Engine) pollRework(ctx context.Context, p core.Project) {
	g, ok := e.GitHub.(core.Feedback)
	if !ok {
		return
	}
	xs, err := g.Labeled(ctx, p, core.ReworkLabel)
	if err != nil {
		_ = e.Store.Event("", "github_error", p.Name+": "+err.Error())
		return
	}
	for _, i := range xs {
		as, _ := core.List[core.Attempt](e.Store, "attempt")
		if prev := latestAttempt(as, p.ID, i.ID); prev != nil {
			e.mu.Lock()
			_, busy := e.workers[prev.ID]
			e.mu.Unlock()
			if busy || prev.Active() {
				continue
			}
		}
		// Claim the request first: a lost label removal must not start rework twice.
		if err := g.RemoveLabel(ctx, p, i.Number, core.ReworkLabel); err != nil {
			_ = e.Store.Event("", "github_error", fmt.Sprintf("%s#%d: %v", p.Name, i.Number, err))
			continue
		}
		if _, err := e.rework(ctx, p, i, "rework"); err != nil {
			body := fmt.Sprintf("### Changathi could not start rework\n\n%s\n\nFix the cause, then add the `%s` label again.", fenced(err.Error()), core.ReworkLabel)
			if postErr := g.StatusComment(ctx, p, i.Number, fmt.Sprintf("rework-%d", i.ID), body); postErr != nil {
				_ = e.Store.Event("", "github_error", fmt.Sprintf("%s#%d: rework refused (%v); posting the reason failed: %v", p.Name, i.Number, err, postErr))
			}
		}
	}
}

// publishStatus mirrors each issue's latest attempt to a status comment when it
// needs a human: stopped in needs_attention, or delivered and awaiting review.
// Only a changed body is written, and a failed write is retried on the next poll.
func (e *Engine) publishStatus(ctx context.Context) {
	g, ok := e.GitHub.(core.Feedback)
	if !ok {
		return
	}
	as, err := core.List[core.Attempt](e.Store, "attempt")
	if err != nil {
		return
	}
	e.mu.Lock()
	operator := e.login
	e.mu.Unlock()
	for _, a := range as {
		if a.State != "needs_attention" && a.State != "awaiting_review" {
			continue
		}
		if latest := latestAttempt(as, a.ProjectID, a.Issue.ID); latest == nil || latest.ID != a.ID {
			continue
		}
		p, err := e.Store.Project(a.ProjectID)
		if err != nil {
			continue
		}
		if operator == "" {
			// Cached for display only; rework reads the login afresh before trusting comments.
			if operator, err = g.Operator(ctx); err != nil {
				return
			}
			e.mu.Lock()
			e.login = operator
			e.mu.Unlock()
		}
		parent := 0
		if a.SupervisorID != "" {
			var v core.Supervisor
			if e.Store.Get("supervisor", a.SupervisorID, &v) == nil {
				parent = v.Parent.Number
			}
		}
		body := statusBody(a, operator, parent)
		digest := sha256.Sum256([]byte(body))
		sum := hex.EncodeToString(digest[:])
		if a.StatusPosted == sum {
			continue
		}
		if err := g.StatusComment(ctx, p, a.Issue.Number, a.ID, body); err != nil {
			_ = e.Store.Event(a.ID, "status_comment", "posting status comment failed: "+err.Error())
			continue
		}
		_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error {
			if x.State == a.State {
				x.StatusPosted = sum
			}
			return nil
		})
	}
}

func statusBody(a core.Attempt, operator string, parent int) string {
	var b strings.Builder
	feedback := fmt.Sprintf("only comments by @%s reach the agent", operator)
	switch a.State {
	case "needs_attention":
		b.WriteString("### Changathi needs attention\n\n")
		fmt.Fprintf(&b, "Generation %d stopped without merging", a.Generation)
		if a.PR != 0 {
			fmt.Fprintf(&b, "; PR #%d is still open", a.PR)
		}
		b.WriteString(".\n\n**Reason**\n\n")
		b.WriteString(fenced(tail(a.Error, 4000)))
		if a.LastFailure != "" && !strings.Contains(a.Error, a.LastFailure) {
			b.WriteString("\n\n**Last failure the agent saw**\n\n")
			b.WriteString(fenced(tail(a.LastFailure, 4000)))
		}
		fmt.Fprintf(&b, "\n\n**Next step:** if the agent can fix this, comment here with guidance (%s), then add the `%s` label. If the cause is outside the code, such as branch protection, credentials or workflow configuration, fix that first and add `%s` without a comment.", feedback, core.ReworkLabel, core.ReworkLabel)
	case "awaiting_review":
		fmt.Fprintf(&b, "### Changathi delivered PR #%d\n\n", a.PR)
		fmt.Fprintf(&b, "Generation %d merged PR #%d. Review the result, then:\n\n", a.Generation, a.PR)
		b.WriteString("- If it meets the acceptance criteria, close this issue as completed.")
		if parent != 0 {
			fmt.Fprintf(&b, " That lets issues that depend on it in supervisor #%d start.", parent)
		}
		fmt.Fprintf(&b, "\n- If something is still missing, comment here with what (%s), then add the `%s` label. Changathi starts a new generation with a new PR and passes your comments to the agent. Do not close and reopen the issue for this.\n", feedback, core.ReworkLabel)
		b.WriteString("- Put separate bugs or follow-up ideas in a new issue that links back here.")
	}
	return b.String()
}

// feedbackText renders the operator's comments created after since, oldest
// first. Changathi's own comments (marked, but posted with the operator's
// login) and evidence approvals are skipped; the newest comments win when the
// size budget runs out. newest is the latest comment time considered.
func feedbackText(comments []core.EvidenceComment, operator string, since time.Time) (string, time.Time) {
	var picked []core.EvidenceComment
	newest := since
	for _, c := range comments {
		body := strings.TrimSpace(c.Body)
		if c.User.Login != operator || c.User.Type != "User" || !c.CreatedAt.After(since) || body == "" || strings.Contains(body, core.CommentMarker) || strings.HasPrefix(body, "/approve-evidence") {
			continue
		}
		picked = append(picked, c)
		if c.CreatedAt.After(newest) {
			newest = c.CreatedAt
		}
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].CreatedAt.Before(picked[j].CreatedAt) })
	const each, total = 8000, 24000
	var parts []string
	size := 0
	for j := len(picked) - 1; j >= 0; j-- {
		body := strings.TrimSpace(picked[j].Body)
		if len(body) > each {
			body = strings.ToValidUTF8(body[:each], "") + "\n[truncated]"
		}
		part := fmt.Sprintf("[%s]\n%s", picked[j].CreatedAt.UTC().Format("2006-01-02 15:04 UTC"), body)
		if size+len(part) > total {
			break
		}
		parts = append([]string{part}, parts...)
		size += len(part)
	}
	if len(parts) == 0 {
		return "", newest
	}
	return "The operator reviewed the earlier work and commented on the issue, oldest first. Treat these comments as the operator's instructions; where they conflict with the original description, follow the comments.\n\n" + strings.Join(parts, "\n\n"), newest
}

func latestAttempt(as []core.Attempt, project string, issue int64) *core.Attempt {
	var latest *core.Attempt
	for _, a := range as {
		if a.ProjectID == project && a.Issue.ID == issue && (latest == nil || a.Generation > latest.Generation) {
			copy := a
			latest = &copy
		}
	}
	return latest
}

// fenced wraps text in a code fence longer than any backtick run inside it.
func fenced(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + "text\n" + strings.TrimRight(text, "\n") + "\n" + fence
}

// tail keeps the end of text, where command failures report their cause.
func tail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return "[…]\n" + strings.ToValidUTF8(text[len(text)-n:], "")
}
