package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Workspaces struct{ Root string }

func (w Workspaces) Clone(p Project) string { return filepath.Join(w.Root, "repos", p.ID) }
func (w Workspaces) Prepare(ctx context.Context, p Project, a Attempt) (string, Workflow, error) {
	flow := a.Workflow
	var e error
	if flow.Revision == "" {
		flow, e = w.LoadWorkflow(ctx, p)
		if e != nil {
			return "", flow, e
		}
	}
	clone := w.Clone(p)
	path := filepath.Join(w.Root, "worktrees", p.ID, a.ID)
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return "", flow, e
	}
	if _, e = os.Stat(path); os.IsNotExist(e) {
		if _, e = Command(ctx, clone, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+a.Branch); e == nil {
			_, e = Command(ctx, clone, "git", "worktree", "add", path, a.Branch)
		} else {
			_, e = Command(ctx, clone, "git", "worktree", "add", "-b", a.Branch, path, flow.Revision)
		}
		if e != nil {
			return "", flow, e
		}
	}
	return path, flow, nil
}
func (w Workspaces) LoadWorkflow(ctx context.Context, p Project) (Workflow, error) {
	clone := w.Clone(p)
	if e := os.MkdirAll(filepath.Dir(clone), 0700); e != nil {
		return Workflow{}, e
	}
	if _, e := os.Stat(clone); os.IsNotExist(e) {
		if _, e = Command(ctx, "", "gh", "repo", "clone", p.Repo, clone, "--", "--no-checkout"); e != nil {
			return Workflow{}, e
		}
	}
	if _, e := Command(ctx, clone, "git", "fetch", "origin", p.Base); e != nil {
		return Workflow{}, e
	}
	base, e := Command(ctx, clone, "git", "rev-parse", "origin/"+p.Base)
	if e != nil {
		return Workflow{}, e
	}
	body, e := Command(ctx, clone, "git", "show", base+":WORKFLOW.md")
	if e != nil {
		return Workflow{}, fmt.Errorf("workflow: add WORKFLOW.md to the repository base branch: %w", e)
	}
	flow, e := ParseWorkflow(body, base)
	if e != nil {
		return flow, fmt.Errorf("workflow: %w", e)
	}
	return flow, nil
}
func (w Workspaces) RefreshBase(ctx context.Context, p Project) (string, error) {
	clone := w.Clone(p)
	if _, e := Command(ctx, clone, "git", "fetch", "origin", p.Base); e != nil {
		return "", e
	}
	return Command(ctx, clone, "git", "rev-parse", "origin/"+p.Base)
}
func (w Workspaces) Clean(ctx context.Context, p Project, a Attempt, apply bool) error {
	if a.Active() {
		return fmt.Errorf("active worktree cannot be removed")
	}
	if a.Workspace == "" {
		return fmt.Errorf("attempt has no worktree")
	}
	status, e := Command(ctx, a.Workspace, "git", "status", "--porcelain")
	if e != nil {
		return e
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("worktree has uncommitted changes")
	}
	if a.State != "done" && a.State != "awaiting_review" && a.State != "superseded" {
		return fmt.Errorf("failed/cancelled worktrees require preservation; cleanup is restricted to delivered work")
	}
	if !apply {
		return nil
	}
	_, e = Command(ctx, w.Clone(p), "git", "worktree", "remove", a.Workspace)
	return e
}
