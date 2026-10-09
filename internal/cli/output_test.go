package cli

import (
	"bytes"
	"changathi/internal/core"
	"encoding/json"
	"strings"
	"testing"
)

func TestReadableAndJSONOutput(t *testing.T) {
	supervisor := core.Supervisor{Parent: core.Issue{Number: 10, Title: "Cluster rollout", Body: "Acceptance criteria"}, State: "waiting_review", Current: 5, Plan: core.SupervisorPlan{Children: []core.SupervisorChild{{Issue: 5, DependsOn: []int{4}}}}, Children: []core.ChildStatus{{Issue: 5, Title: "Install services", State: "awaiting_review", PR: 12}}}
	for _, value := range []any{supervisor, core.Project{Name: "homelab"}, core.Attempt{ID: "worker", State: "running"}, map[string]any{"removed": false, "path": "/tmp/work", "usage": map[string]any{"usedPercent": 12}}, "stopping"} {
		var b bytes.Buffer
		a := &app{out: &b}
		if err := a.display(value); err != nil {
			t.Fatal(err)
		}
		if b.Len() == 0 || strings.HasPrefix(b.String(), "{") || strings.HasPrefix(b.String(), "\"") {
			t.Fatal(b.String())
		}
		b.Reset()
		a.json = true
		if err := a.display(value); err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(value)
		var compact bytes.Buffer
		if err := json.Compact(&compact, b.Bytes()); err != nil {
			t.Fatal(err)
		}
		if compact.String() != string(want) {
			t.Fatal("JSON changed")
		}
	}
	var b bytes.Buffer
	a := &app{out: &b}
	if err := a.display(supervisor); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Supervisor #10", "Cluster rollout", "Acceptance criteria", "Install services", "#4", "#12", "waiting review"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q: %s", want, b.String())
		}
	}
	b.Reset()
	if err := a.display(map[string]any{"text": "\x1b[31mhello\x1b[0m"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "\x1b") {
		t.Fatal("terminal escape leaked")
	}
}
