package cli

import (
	"changathi/internal/core"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// agentRow is the compact view of an attempt. It leaves out the recorded
// workflow, prompt and provider identifiers, which callers rarely need and
// which dominate the size of a full attempt.
type agentRow struct {
	ID      string    `json:"id"`
	Issue   string    `json:"issue"`
	Title   string    `json:"title"`
	State   string    `json:"state"`
	PR      int       `json:"pr,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Updated time.Time `json:"updated"`
}

type agentFilter struct {
	active  bool
	project string
	limit   int
}

func projectNames(s core.Snapshot) map[string]string {
	names := map[string]string{}
	for _, p := range s.Projects {
		names[p.ID] = p.Name
	}
	return names
}

func newAgentRow(v core.Attempt, names map[string]string) agentRow {
	detail := singleLine(v.Error)
	if detail == "" {
		detail = singleLine(v.Activity)
	}
	if r := []rune(detail); len(r) > 160 {
		detail = string(r[:160]) + "…"
	}
	return agentRow{ID: v.ID, Issue: fmt.Sprintf("%s#%d", names[v.ProjectID], v.Issue.Number), Title: singleLine(v.Issue.Title), State: v.State, PR: v.PR, Detail: detail, Updated: v.Updated}
}

// agentRows returns attempts newest first, skipping superseded generations.
func agentRows(s core.Snapshot, f agentFilter) []agentRow {
	names := projectNames(s)
	out := []agentRow{}
	for _, v := range s.Agents {
		if v.State == "superseded" || (f.active && !v.Active()) || (f.project != "" && names[v.ProjectID] != f.project) {
			continue
		}
		out = append(out, newAgentRow(v, names))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	if f.limit > 0 && len(out) > f.limit {
		out = out[:f.limit]
	}
	return out
}

func agentDetail(v core.Attempt, full bool) any {
	if full {
		return v
	}
	data, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	delete(m, "workflow")
	if issue, ok := m["issue"].(map[string]any); ok {
		delete(issue, "body")
	}
	return m
}

func (a *app) agentRows(rows []agentRow) error {
	if a.json {
		return a.print(rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(a.out, "No agents.")
		return err
	}
	w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tISSUE\tSUBJECT\tSTATE\tPR\tDETAIL")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", r.ID, r.Issue, r.Title, r.State, r.PR, r.Detail)
	}
	return w.Flush()
}

type supervisorRow struct {
	Issue  string `json:"issue"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type messageRow struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// summaryView answers "what is running and what needs a human?" in one call.
type summaryView struct {
	ProviderWait   *core.ProviderGate `json:"provider_wait,omitempty"`
	PausedProjects []string           `json:"paused_projects,omitempty"`
	Running        []agentRow         `json:"running"`
	NeedsAttention []agentRow         `json:"needs_attention"`
	AwaitingReview []agentRow         `json:"awaiting_review"`
	Supervisors    []supervisorRow    `json:"supervisors,omitempty"`
	Messages       []messageRow       `json:"messages,omitempty"`
}

func summarize(s core.Snapshot) summaryView {
	names := projectNames(s)
	v := summaryView{ProviderWait: s.ProviderWait, Running: []agentRow{}, NeedsAttention: []agentRow{}, AwaitingReview: []agentRow{}}
	for _, p := range s.Projects {
		if !p.Enabled {
			v.PausedProjects = append(v.PausedProjects, p.Name)
		}
	}
	// Only the newest attempt for each issue reflects its current state.
	latest := map[string]core.Attempt{}
	for _, a := range s.Agents {
		key := fmt.Sprintf("%s#%d", a.ProjectID, a.Issue.Number)
		if old, ok := latest[key]; !ok || a.Generation > old.Generation || (a.Generation == old.Generation && a.Created.After(old.Created)) {
			latest[key] = a
		}
	}
	for _, a := range latest {
		row := newAgentRow(a, names)
		switch {
		case a.Active():
			v.Running = append(v.Running, row)
		case a.State == "needs_attention":
			v.NeedsAttention = append(v.NeedsAttention, row)
		case a.State == "awaiting_review":
			v.AwaitingReview = append(v.AwaitingReview, row)
		}
	}
	for _, rows := range [][]agentRow{v.Running, v.NeedsAttention, v.AwaitingReview} {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Updated.After(rows[j].Updated) })
	}
	for _, sv := range s.Supervisors {
		if sv.State == "done" || sv.State == "cancelled" {
			continue
		}
		v.Supervisors = append(v.Supervisors, supervisorRow{Issue: fmt.Sprintf("%s#%d", names[sv.ProjectID], sv.Parent.Number), Title: singleLine(sv.Parent.Title), State: sv.State, Reason: singleLine(sv.Reason)})
	}
	for _, m := range s.Messages {
		if m.State == "queued" || m.State == "uncertain" {
			v.Messages = append(v.Messages, messageRow{ID: m.ID, Agent: m.AttemptID, State: m.State, Error: singleLine(m.Error)})
		}
	}
	return v
}

func (a *app) summary() *cobra.Command {
	return &cobra.Command{Use: "summary", Short: "What is running and what needs a human", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		s, err := a.client().Status(c.Context())
		if err != nil {
			return err
		}
		v := summarize(s)
		if a.json {
			return a.print(v)
		}
		return a.printSummary(v)
	}}
}

func (a *app) printSummary(v summaryView) error {
	if v.ProviderWait != nil {
		fmt.Fprintf(a.out, "Provider paused until %s: %s\n", v.ProviderWait.Until.Local().Format(time.RFC3339), singleLine(v.ProviderWait.Reason))
	}
	if len(v.PausedProjects) > 0 {
		fmt.Fprintf(a.out, "Paused projects: %s\n", strings.Join(v.PausedProjects, ", "))
	}
	section := func(title string, rows []agentRow) error {
		fmt.Fprintf(a.out, "\n%s (%d)\n", title, len(rows))
		if len(rows) == 0 {
			return nil
		}
		w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		for _, r := range rows {
			pr := ""
			if r.PR != 0 {
				pr = fmt.Sprintf("PR #%d", r.PR)
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n", r.Issue, r.ID, r.State, pr, r.Title, r.Detail)
		}
		return w.Flush()
	}
	if err := section("Needs attention", v.NeedsAttention); err != nil {
		return err
	}
	if err := section("Awaiting review", v.AwaitingReview); err != nil {
		return err
	}
	if err := section("Running", v.Running); err != nil {
		return err
	}
	if len(v.Supervisors) > 0 {
		fmt.Fprintf(a.out, "\nSupervisors (%d)\n", len(v.Supervisors))
		for _, s := range v.Supervisors {
			fmt.Fprintf(a.out, "  %s  %s  %s  %s\n", s.Issue, s.State, s.Title, s.Reason)
		}
	}
	if len(v.Messages) > 0 {
		fmt.Fprintf(a.out, "\nMessages awaiting delivery (%d)\n", len(v.Messages))
		for _, m := range v.Messages {
			fmt.Fprintf(a.out, "  %s  agent %s  %s  %s\n", m.ID, m.Agent, m.State, m.Error)
		}
	}
	return nil
}
