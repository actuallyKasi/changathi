package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSupervisorGitHubEvidenceAndWorkpadRecovery(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	// Emulate a closure/reopen history and an existing workpad from a lost POST response.
	script := `#!/bin/sh
case "$2" in
 user) printf '%s' '{"login":"operator","type":"User"}' ;;
 *events*) printf '%s' '[{"id":1,"event":"closed","actor":{"login":"operator","type":"User"},"created_at":"2026-01-01T00:00:00Z"},{"id":2,"event":"reopened","created_at":"2026-01-02T00:00:00Z"}]' ;;
 *issues/1/sub_issues*) printf '%s' '[{"id":300,"number":3,"repository_url":"https://api.github.com/repos/owner/repo"}]' ;;
 *issues/3/dependencies/blocked_by*) printf '%s' '[]' ;;
 *pulls/9/files*) printf '%s' '[{"filename":"docs/validation/run.md","status":"added"},{"filename":"README.md","status":"modified"}]' ;;
 *pulls/8/files*) printf '%s' '[{"filename":"main.go","status":"modified"}]' ;;
 *pulls/8) printf '%s' '{"number":8,"body":"Done.\nEvidence: https://example.test/run","head":{"sha":"def"}}' ;;
 *pulls/7/files*) printf '%s' '[]' ;;
 *pulls/7) printf '%s' '{"number":7,"body":"no evidence","head":{"sha":"fed"}}' ;;
 *pulls/9) printf '%s' '{"number":9,"merged_at":"2026-01-01T00:00:00Z","head":{"sha":"abc"},"base":{"ref":"main","repo":{"id":42}}}' ;;
 *issues/1/comments\?*) printf '%s' '[{"id":77,"body":"<!-- changathi-supervisor:test-plan -->","user":{"login":"operator"}}]' ;;
 *issues/comments/77) printf '%s\n' "$4" >> "$TEST_GH_CALLS"; printf '%s' '{"id":77}' ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_GH_CALLS", log)
	g := GH{}
	p := Project{Repo: "owner/repo", RepoID: 42, Base: "main"}
	ctx := context.Background()
	cl, err := g.Closure(ctx, p, 1)
	if err != nil || cl.Event != "reopened" {
		t.Fatal(cl, err)
	}
	pr, err := g.Delivery(ctx, p, 9)
	if err != nil || pr.Base.Repo.ID != 42 || pr.MergedAt.IsZero() {
		t.Fatal(pr, err)
	}
	subs, err := g.SubIssues(ctx, p, 1)
	if err != nil || len(subs) != 1 || subs[0].Number != 3 || !subs[0].InRepo("owner/repo") {
		t.Fatal(subs, err)
	}
	if blocked, err := g.BlockedBy(ctx, p, 3); err != nil || len(blocked) != 0 {
		t.Fatal(blocked, err)
	}
	for pr, want := range map[int]string{
		9: "https://github.com/owner/repo/blob/abc/docs/validation/run.md",
		8: "https://example.test/run",
		7: "",
	} {
		if got, err := g.EvidenceReport(ctx, p, pr); err != nil || got != want {
			t.Fatalf("PR #%d: %q %v", pr, got, err)
		}
	}
	id, err := g.SupervisorWorkpad(ctx, p, 1, "test-plan", "updated status")
	if err != nil || id != 77 {
		t.Fatal(id, err)
	}
	b, _ := os.ReadFile(log)
	if string(b) != "PATCH\n" {
		t.Fatalf("did not recover existing workpad: %q", b)
	}
}
