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
	id, err := g.SupervisorWorkpad(ctx, p, 1, "test-plan", "updated status")
	if err != nil || id != 77 {
		t.Fatal(id, err)
	}
	b, _ := os.ReadFile(log)
	if string(b) != "PATCH\n" {
		t.Fatalf("did not recover existing workpad: %q", b)
	}
}
