package core

import "testing"

func TestWorkflow(t *testing.T) {
	w, e := ParseWorkflow("---\nvalidation:\n  - go test ./...\nmax_turns: 3\n---\nImplement {{.Issue.Title}}", "sha")
	if e != nil {
		t.Fatal(e)
	}
	s, e := w.Render(Issue{Title: "feature"})
	if e != nil || s != "Implement feature" {
		t.Fatalf("%q %v", s, e)
	}
	for _, bad := range []string{"---\nvalidation: [true]\nunknown: 2\n---\nbody", "---\nvalidation: [go test]\nmax_turns: 0\n---\nbody", "---\nvalidation: [go test]", "body"} {
		if _, e = ParseWorkflow(bad, "sha"); e == nil {
			t.Fatalf("accepted invalid workflow %s", bad)
		}
	}
	w, e = ParseWorkflow("---\nvalidation: [go test]\n---\n{{.Missing}}", "sha")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = w.Render(Issue{}); e == nil {
		t.Fatal("unknown template variable accepted")
	}
}
