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

// replyInstructions tells a rework agent how to answer a question without
// changing code. Without it, the no-commit validation check pushes the agent to
// invent a change.
const replyInstructions = "Some comments only ask a question, or need no code change once you have checked. In that case do not edit or commit anything just to give Changathi a commit. Leave the branch as it is and end your final message with exactly one fenced block holding your answer for the operator. Changathi posts it on the issue and returns the issue to review, with no validation and no PR:\n````changathi-reply\nYour answer, in Markdown.\n````\nIf the comments ask for changes, implement and commit them as usual and leave that block out. A turn that changes the branch is validated and delivered as usual, even if it includes the block."

// parseReply extracts the last changathi-reply block from an agent message.
// The closing fence is the last line made of the opening fence, so the answer
// may contain shorter code fences of its own.
func parseReply(text string) (string, bool) {
	at := strings.LastIndex(text, "changathi-reply")
	if at < 3 {
		return "", false
	}
	start := at
	for start > 0 && text[start-1] == '`' {
		start--
	}
	fence := text[start:at]
	if len(fence) < 3 || (start > 0 && text[start-1] != '\n') {
		return "", false
	}
	rest := text[at+len("changathi-reply"):]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 || strings.TrimSpace(rest[:nl]) != "" {
		return "", false
	}
	body := rest[nl+1:]
	end, off := -1, 0
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.TrimSpace(line) == fence {
			end = off
		}
		off += len(line)
	}
	if end < 0 {
		return "", false
	}
	reply := strings.TrimSpace(body[:end])
	return reply, reply != ""
}

// worktreeState is HEAD plus uncommitted changes, or "" when git fails, so a
// reply is accepted only from a turn that left the branch untouched.
func worktreeState(ctx context.Context, path string) string {
	head, err := core.Command(ctx, path, "git", "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	status, err := core.Command(ctx, path, "git", "status", "--porcelain")
	if err != nil {
		return ""
	}
	return head + "\n" + status
}

// answer ends a rework turn that replied to the operator without changing
// code. The reply goes to the transcript and the issue. An attempt that holds
// no work of its own returns to review of the merged generation before it;
// otherwise its undelivered work needs the operator again. Nothing is
// validated, verified or opened as a PR.
func (e *Engine) answer(ctx context.Context, p core.Project, a core.Attempt, reply string) {
	e.event(a.ID, "reply", reply)
	var prev *core.Attempt
	ahead, err := core.Command(ctx, a.Workspace, "git", "rev-list", "--count", "origin/"+p.Base+"..HEAD")
	if err == nil && ahead == "0" && a.PR == 0 && a.Previous != nil && a.Previous.PR != 0 {
		as, _ := core.List[core.Attempt](e.Store, "attempt")
		for _, x := range as {
			if x.ProjectID == a.ProjectID && x.Issue.ID == a.Issue.ID && x.Generation == a.Previous.Generation && x.PR != 0 && (x.State == "awaiting_review" || x.State == "done" || x.State == "superseded") {
				copy := x
				prev = &copy
			}
		}
	}
	_ = e.Store.UpdateAttempt(a.ID, func(x *core.Attempt) error {
		x.Reply, x.NextStep = reply, ""
		if prev != nil {
			// The merged generation stays the delivery under review, so the
			// supervisor's evidence gate still finds its head.
			x.PR, x.Head, x.BaseSHA = prev.PR, prev.Head, prev.BaseSHA
		}
		return nil
	})
	if prev != nil {
		if err := e.GitHub.MarkReview(ctx, p, a.Issue.Number); err != nil {
			e.event(a.ID, "reply", "restoring agent:review failed: "+err.Error())
		}
		e.state(a.ID, "awaiting_review", "")
	} else {
		e.state(a.ID, "needs_attention", "the agent answered the operator's comments without changing code; this generation's work is still undelivered")
	}
	e.publishStatus(ctx)
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
		// Each reply is its own comment, so a later one never erases an earlier answer.
		key := a.ID
		if a.Reply != "" {
			key += "-reply-" + sum[:12]
		}
		if err := g.StatusComment(ctx, p, a.Issue.Number, key, body); err != nil {
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
	if a.Reply != "" {
		fmt.Fprintf(&b, "### Changathi agent reply\n\nGeneration %d answered your comments without changing code.\n\n", a.Generation)
		b.WriteString(a.Reply)
		b.WriteString("\n\n---\n\n")
		if a.State == "awaiting_review" {
			fmt.Fprintf(&b, "**Next step:** PR #%d is still the delivered work under review. Close this issue as completed if it meets the acceptance criteria. ", a.PR)
		} else {
			b.WriteString("**Next step:** this generation's work is still undelivered. ")
		}
		fmt.Fprintf(&b, "If you want changes, comment with what (%s), then add the `%s` label. A question that needs no code change doesn't need the label; ask it in your Changathi operator session instead.", feedback, core.ReworkLabel)
		return b.String()
	}
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
