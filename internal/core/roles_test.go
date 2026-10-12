package core

import (
	"strings"
	"testing"
)

func TestWorkflowAgents(t *testing.T) {
	w, err := ParseWorkflow("---\nagents:\n  implement:\n    provider: claude\n    model: claude-opus-5-5\n    effort: high\n  verify:\n    provider: claude\n    model: claude-sonnet-5-5\n    instructions: Be strict.\nvalidation: [go test]\nmax_verify_rounds: 3\n---\nbody", "sha")
	if err != nil {
		t.Fatal(err)
	}
	if r := w.Implementer(); r.Provider != "claude" || r.Model != "claude-opus-5-5" || r.Effort != "high" {
		t.Fatalf("implement %+v", r)
	}
	if v := w.Verifier(); v == nil || v.Model != "claude-sonnet-5-5" || v.Instructions != "Be strict." || w.VerifyRounds() != 3 {
		t.Fatalf("verify %+v rounds %d", v, w.VerifyRounds())
	}
	legacy, err := ParseWorkflow("---\nmodel: gpt-5\nvalidation: [go test]\n---\nbody", "sha")
	if err != nil || legacy.Implementer() != (Role{Provider: "codex", Model: "gpt-5"}) || legacy.Verifier() != nil || legacy.VerifyRounds() != 2 {
		t.Fatalf("legacy %+v %v", legacy, err)
	}
	if (Workflow{}).VerifyRounds() != 2 {
		t.Fatal("stored workflows without max_verify_rounds need the default")
	}
	head := "---\nvalidation: [go test]\n"
	for name, bad := range map[string]string{
		"both model and agents":  "model: x\nagents:\n  implement:\n    provider: codex\n",
		"missing implement":      "agents:\n  verify:\n    provider: codex\n",
		"claude without model":   "agents:\n  implement:\n    provider: claude\n",
		"unknown provider":       "agents:\n  implement:\n    provider: gemini\n",
		"bad effort":             "agents:\n  implement:\n    provider: claude\n    model: m\n    effort: extreme\n",
		"codex effort":           "agents:\n  implement:\n    provider: codex\n    effort: high\n",
		"implement instructions": "agents:\n  implement:\n    provider: codex\n    instructions: x\n",
		"unknown role key":       "agents:\n  implement:\n    provider: codex\n    temperature: 1\n",
		"unknown role":           "agents:\n  implement:\n    provider: codex\n  plan:\n    provider: codex\n",
		"zero verify rounds":     "max_verify_rounds: 0\n",
	} {
		if _, err := ParseWorkflow(head+bad+"---\nbody", "sha"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestConfigAllows(t *testing.T) {
	c := DefaultConfig()
	if c.Allows(Role{Provider: "codex", Model: "anything"}) != nil || c.Allows(Role{Provider: "claude", Model: "claude-opus-5-5"}) == nil {
		t.Fatal("default config must keep Codex-only behavior")
	}
	c.Providers = map[string]ProviderPolicy{"claude": {Models: []string{"claude-sonnet-5-5"}}}
	if c.Allows(Role{Provider: "claude", Model: "claude-sonnet-5-5"}) != nil {
		t.Fatal("allowed model rejected")
	}
	if c.Allows(Role{Provider: "claude", Model: "claude-opus-5-5"}) == nil || c.Allows(Role{Provider: "codex"}) == nil {
		t.Fatal("allow-list widened")
	}
	c.Providers["gemini"] = ProviderPolicy{}
	if c.Validate() == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestParseVerdict(t *testing.T) {
	good := map[string]string{
		"structured": `{"verdict":"pass","findings":[]}`,
		"fenced":     "Looks fine.\n```changathi-verdict\n{\"verdict\":\"fail\",\"findings\":[{\"severity\":\"blocking\",\"summary\":\"no test\"}]}\n```\n",
		"notes":      `{"verdict":"pass","findings":[{"severity":"note","file":"a.go","line":3,"summary":"nit"}]}`,
	}
	for name, text := range good {
		if _, err := ParseVerdict(text); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"prose":             "Verdict: pass",
		"empty":             "",
		"pass with blocker": `{"verdict":"pass","findings":[{"severity":"blocking","summary":"x"}]}`,
		"fail without":      `{"verdict":"fail","findings":[]}`,
		"unknown verdict":   `{"verdict":"maybe","findings":[]}`,
		"unknown field":     `{"verdict":"pass","findings":[],"confidence":1}`,
		"bad severity":      `{"verdict":"pass","findings":[{"severity":"minor","summary":"x"}]}`,
		"empty summary":     `{"verdict":"pass","findings":[{"severity":"note","summary":" "}]}`,
		"two blocks":        strings.Repeat("```changathi-verdict\n{\"verdict\":\"pass\",\"findings\":[]}\n```\n", 2),
		"trailing":          `{"verdict":"pass","findings":[]} {"verdict":"fail"}`,
	}
	for name, text := range bad {
		if _, err := ParseVerdict(text); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	a := Attempt{Head: "h2", Verifications: []Verification{{Head: "h1", Verdict: "pass", Findings: []Finding{{Severity: "note", Summary: "old"}}}, {Head: "h2", Verdict: "pass", Findings: []Finding{{Severity: "note", File: "a.go", Line: 3, Summary: "new"}}}}}
	if a.VerificationNotes() != "- a.go:3: new\n" {
		t.Fatalf("notes %q", a.VerificationNotes())
	}
}
