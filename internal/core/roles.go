package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Providers lists the agent providers Changathi can run.
var Providers = []string{"codex", "claude"}

func KnownProvider(name string) bool {
	for _, p := range Providers {
		if p == name {
			return true
		}
	}
	return false
}

// Role selects the provider and model for one kind of agent work.
type Role struct {
	Provider     string `json:"provider" yaml:"provider"`
	Model        string `json:"model,omitempty" yaml:"model"`
	Effort       string `json:"effort,omitempty" yaml:"effort"`
	Instructions string `json:"instructions,omitempty" yaml:"instructions"`
}

// Agents maps workflow roles to providers. Implement is required; Verify enables
// an independent review of each validated head before publication.
type Agents struct {
	Implement *Role `json:"implement,omitempty" yaml:"implement"`
	Verify    *Role `json:"verify,omitempty" yaml:"verify"`
}

var efforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

func (r Role) validate(name string) error {
	if !KnownProvider(r.Provider) {
		return fmt.Errorf("agents.%s.provider must be one of %s", name, strings.Join(Providers, ", "))
	}
	if r.Provider == "claude" && r.Model == "" {
		return fmt.Errorf("agents.%s.model is required for the claude provider", name)
	}
	if r.Effort != "" {
		if r.Provider != "claude" {
			return fmt.Errorf("agents.%s.effort is only supported by the claude provider", name)
		}
		if !efforts[r.Effort] {
			return fmt.Errorf("agents.%s.effort must be low, medium, high, xhigh or max", name)
		}
	}
	if r.Instructions != "" && name != "verify" {
		return fmt.Errorf("agents.%s.instructions is only valid for the verify role", name)
	}
	return nil
}

// Implementer resolves the implement role, mapping the legacy top-level model to Codex.
func (w Workflow) Implementer() Role {
	if w.Agents != nil && w.Agents.Implement != nil {
		return *w.Agents.Implement
	}
	return Role{Provider: "codex", Model: w.Model}
}

// Verifier returns the verify role, or nil when verification is disabled.
func (w Workflow) Verifier() *Role {
	if w.Agents == nil {
		return nil
	}
	return w.Agents.Verify
}

// VerifyRounds returns the failing-verdict limit, defaulting records written before it existed.
func (w Workflow) VerifyRounds() int {
	if w.MaxVerifyRounds < 1 {
		return 2
	}
	return w.MaxVerifyRounds
}

// Roles lists every configured role, for allow-list and diagnostic checks.
func (w Workflow) Roles() map[string]Role {
	out := map[string]Role{"implement": w.Implementer()}
	if v := w.Verifier(); v != nil {
		out["verify"] = *v
	}
	return out
}

func (w Workflow) validateAgents() error {
	if w.Agents == nil {
		return nil
	}
	if w.Model != "" {
		return errors.New("set either model or agents, not both")
	}
	if w.Agents.Implement == nil {
		return errors.New("agents.implement is required")
	}
	if e := w.Agents.Implement.validate("implement"); e != nil {
		return e
	}
	if w.Agents.Verify != nil {
		return w.Agents.Verify.validate("verify")
	}
	return nil
}

// PreviousGeneration carries a superseded generation's outcome into its rework.
type PreviousGeneration struct {
	Generation  int    `json:"generation"`
	PR          int    `json:"pr,omitempty"`
	State       string `json:"state"`
	LastFailure string `json:"last_failure,omitempty"`
}

// Verification records one verifier verdict for a validated head.
type Verification struct {
	Head     string    `json:"head"`
	Provider string    `json:"provider"`
	Model    string    `json:"model,omitempty"`
	Session  string    `json:"session,omitempty"`
	Verdict  string    `json:"verdict"` // pass, fail, or malformed
	Findings []Finding `json:"findings,omitempty"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created"`
}
type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Summary  string `json:"summary"`
}
type Verdict struct {
	Verdict  string    `json:"verdict"`
	Findings []Finding `json:"findings"`
}

// VerdictSchema is passed to providers that support structured output.
const VerdictSchema = `{"type":"object","additionalProperties":false,"required":["verdict","findings"],"properties":{"verdict":{"type":"string","enum":["pass","fail"]},"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["severity","summary"],"properties":{"severity":{"type":"string","enum":["blocking","note"]},"file":{"type":"string"},"line":{"type":"integer"},"summary":{"type":"string"}}}}}}`

var verdictBlock = regexp.MustCompile("(?s)```changathi-verdict[ \t]*\n(.*?)\n```")

// ParseVerdict accepts a structured-output JSON document or exactly one fenced
// changathi-verdict block. Anything ambiguous or inconsistent is an error, never a pass.
func ParseVerdict(text string) (Verdict, error) {
	var v Verdict
	body := strings.TrimSpace(text)
	if blocks := verdictBlock.FindAllStringSubmatch(text, -1); len(blocks) > 1 {
		return v, errors.New("more than one verdict block")
	} else if len(blocks) == 1 {
		body = blocks[0][1]
	} else if !strings.HasPrefix(body, "{") {
		return v, errors.New("no verdict found")
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&v); e != nil {
		return v, fmt.Errorf("invalid verdict: %w", e)
	}
	if dec.More() {
		return v, errors.New("trailing data after verdict")
	}
	blocking := 0
	for _, f := range v.Findings {
		switch f.Severity {
		case "blocking":
			blocking++
		case "note":
		default:
			return v, fmt.Errorf("invalid finding severity %q", f.Severity)
		}
		if strings.TrimSpace(f.Summary) == "" {
			return v, errors.New("finding without summary")
		}
	}
	switch v.Verdict {
	case "pass":
		if blocking > 0 {
			return v, errors.New("pass verdict has blocking findings")
		}
	case "fail":
		if blocking == 0 {
			return v, errors.New("fail verdict has no blocking findings")
		}
	default:
		return v, fmt.Errorf("invalid verdict %q", v.Verdict)
	}
	return v, nil
}

// VerificationNotes returns the note findings of the passing verdict for the attempt's head.
func (a Attempt) VerificationNotes() string {
	for i := len(a.Verifications) - 1; i >= 0; i-- {
		if v := a.Verifications[i]; v.Head == a.Head {
			if v.Verdict != "pass" {
				return ""
			}
			return FindingsText(v.Findings, "note")
		}
	}
	return ""
}

// FindingsText renders findings of one severity for prompts and PR bodies.
func FindingsText(fs []Finding, severity string) string {
	var b strings.Builder
	for _, f := range fs {
		if f.Severity != severity {
			continue
		}
		b.WriteString("- ")
		if f.File != "" {
			b.WriteString(f.File)
			if f.Line > 0 {
				fmt.Fprintf(&b, ":%d", f.Line)
			}
			b.WriteString(": ")
		}
		b.WriteString(strings.TrimSpace(f.Summary))
		b.WriteString("\n")
	}
	return b.String()
}
