package cli

import (
	"bytes"
	"changathi/internal/core"
	"changathi/skills"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func summaryFixture() core.Snapshot {
	at := func(id string, n, gen int, state string, minute int) core.Attempt {
		return core.Attempt{ID: id, ProjectID: "p1", Issue: core.Issue{Number: n, Title: "issue", Body: "long body"}, Generation: gen, State: state,
			Workflow: core.Workflow{Prompt: "big prompt"}, Created: time.Unix(int64(minute*60), 0), Updated: time.Unix(int64(minute*60), 0)}
	}
	return core.Snapshot{
		Projects: []core.Project{{ID: "p1", Name: "backend", Enabled: true}, {ID: "p2", Name: "frontend"}},
		Agents: []core.Attempt{
			at("old", 1, 1, "superseded", 1), at("new", 1, 2, "awaiting_review", 2),
			at("run", 2, 1, "running", 3), at("stuck", 3, 1, "needs_attention", 4), at("done", 4, 1, "done", 5),
		},
		Supervisors: []core.Supervisor{{ProjectID: "p1", Parent: core.Issue{Number: 9}, State: "waiting_review"}, {ProjectID: "p1", Parent: core.Issue{Number: 8}, State: "done"}},
		Messages:    []core.Message{{ID: "m1", AttemptID: "run", State: "uncertain"}, {ID: "m2", AttemptID: "run", State: "acknowledged"}},
	}
}

func TestSummaryReportsOnlyCurrentWorkNeedingAction(t *testing.T) {
	v := summarize(summaryFixture())
	ids := func(rows []agentRow) string {
		out := []string{}
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}
	if ids(v.Running) != "run" || ids(v.NeedsAttention) != "stuck" || ids(v.AwaitingReview) != "new" {
		t.Fatalf("%+v", v)
	}
	if len(v.Supervisors) != 1 || v.Supervisors[0].Issue != "backend#9" || len(v.Messages) != 1 || v.Messages[0].ID != "m1" {
		t.Fatalf("%+v", v)
	}
	if strings.Join(v.PausedProjects, ",") != "frontend" {
		t.Fatal(v.PausedProjects)
	}
}

func TestAgentRowsAreCompactAndFiltered(t *testing.T) {
	s := summaryFixture()
	rows := agentRows(s, agentFilter{limit: 2})
	if len(rows) != 2 || rows[0].ID != "done" || rows[1].ID != "stuck" {
		t.Fatalf("%+v", rows)
	}
	if rows := agentRows(s, agentFilter{active: true}); len(rows) != 1 || rows[0].ID != "run" || rows[0].Issue != "backend#2" {
		t.Fatalf("%+v", rows)
	}
	for _, r := range agentRows(s, agentFilter{}) {
		if r.ID == "old" {
			t.Fatal("superseded attempt listed")
		}
	}
	var out bytes.Buffer
	a := &app{out: &out, json: true}
	if err := a.display(agentDetail(s.Agents[2], false)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "big prompt") || strings.Contains(out.String(), "long body") {
		t.Fatal(out.String())
	}
}

func TestSkillInstall(t *testing.T) {
	dir := t.TempDir()
	a := &app{out: &bytes.Buffer{}}
	cmd := a.skill()
	cmd.SetArgs([]string{"install", "--dir", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".claude", "skills", "changathi", "SKILL.md"))
	if err != nil || string(b) != skills.Changathi || !strings.HasPrefix(string(b), "---\nname: changathi\n") {
		t.Fatal(err)
	}
}
