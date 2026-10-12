package core

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The shipped examples must stay valid, including the commented agents block.
func TestExamplesParse(t *testing.T) {
	b, err := os.ReadFile("../../examples/WORKFLOW.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseWorkflow(string(b), "sha"); err != nil {
		t.Fatal(err)
	}
	uncommented := regexp.MustCompile(`(?m)^# (agents:|  |max_verify_rounds)`).ReplaceAllString(string(b), "$1")
	w, err := ParseWorkflow(uncommented, "sha")
	if err != nil {
		t.Fatalf("agents example: %v", err)
	}
	if w.Implementer().Model != "claude-opus-5-5" || w.Verifier() == nil || w.Verifier().Model != "claude-sonnet-5-5" {
		t.Fatalf("agents example roles %+v", w.Agents)
	}
	b, err = os.ReadFile("../../examples/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := DefaultConfig()
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		t.Fatal(err)
	}
	for _, r := range w.Roles() {
		if err = c.Allows(r); err != nil {
			t.Fatalf("example config rejects example workflow: %v", err)
		}
	}
}
