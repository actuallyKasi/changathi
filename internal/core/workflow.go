package core

import (
	"bytes"
	"fmt"
	"gopkg.in/yaml.v3"
	"strings"
	"text/template"
)

type Workflow struct {
	Model          string   `json:"model,omitempty" yaml:"model"`
	Validation     []string `json:"validation" yaml:"validation"`
	RequiredChecks []string `json:"required_checks" yaml:"required_checks"`
	MergeMethod    string   `json:"merge_method" yaml:"merge_method"`
	MaxTurns       int      `json:"max_turns" yaml:"max_turns"`
	TimeoutSeconds int      `json:"timeout_seconds" yaml:"timeout_seconds"`
	Concurrency    int      `json:"concurrency" yaml:"concurrency"`
	Prompt         string   `json:"prompt" yaml:"-"`
	Revision       string   `json:"revision" yaml:"-"`
}

func ParseWorkflow(data, revision string) (Workflow, error) {
	w := Workflow{MergeMethod: "squash", MaxTurns: 5, TimeoutSeconds: 3600, Concurrency: 1, Revision: revision}
	body := data
	if strings.HasPrefix(data, "---\n") {
		parts := strings.SplitN(data[4:], "\n---", 2)
		if len(parts) != 2 {
			return w, fmt.Errorf("unterminated workflow front matter")
		}
		dec := yaml.NewDecoder(strings.NewReader(parts[0]))
		dec.KnownFields(true)
		if e := dec.Decode(&w); e != nil {
			return w, e
		}
		body = strings.TrimPrefix(parts[1], "\n")
	}
	w.Prompt = strings.TrimSpace(body)
	if len(w.Validation) == 0 {
		return w, fmt.Errorf("WORKFLOW.md requires at least one validation command")
	}
	if w.MaxTurns < 1 || w.TimeoutSeconds < 1 || w.Concurrency < 1 {
		return w, fmt.Errorf("invalid workflow limits")
	}
	if w.MergeMethod != "squash" && w.MergeMethod != "merge" && w.MergeMethod != "rebase" {
		return w, fmt.Errorf("invalid merge method")
	}
	_, e := template.New("workflow").Option("missingkey=error").Parse(w.Prompt)
	return w, e
}
func (w Workflow) Render(i Issue) (string, error) {
	t, e := template.New("workflow").Option("missingkey=error").Parse(w.Prompt)
	if e != nil {
		return "", e
	}
	var b bytes.Buffer
	e = t.Execute(&b, map[string]any{"Issue": i})
	return b.String(), e
}
