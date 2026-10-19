package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"sort"
	"strings"
	"time"
)

type SupervisorChild struct {
	Issue            int    `json:"issue" yaml:"issue"`
	DependsOn        []int  `json:"depends_on" yaml:"depends_on"`
	Completion       string `json:"completion" yaml:"completion"`
	RequiresEvidence bool   `json:"requires_evidence,omitempty" yaml:"requires_evidence,omitempty"`
}
type SupervisorPlan struct {
	Version  int               `json:"version" yaml:"version"`
	Children []SupervisorChild `json:"children" yaml:"children"`
	// Source is PlanSubIssues or PlanBody; it is part of the revision hash.
	Source string `json:"source,omitempty" yaml:"-"`
}

// Plan sources. Sub-issues with blocked-by links are the native form; the fenced
// YAML block in the parent body is the deprecated form kept for existing plans.
const (
	PlanSubIssues = "sub_issues"
	PlanBody      = "body"
)

// Child policy labels for plans read from sub-issues.
const (
	EvidenceLabel = "changathi:evidence"
	ManualLabel   = "changathi:manual"
)

// EvidenceDir is where a delivery's committed validation report lives.
const EvidenceDir = "docs/validation/"

type ChildEvidence struct {
	IssueID        int64     `json:"issue_id"`
	PR             int       `json:"pr,omitempty"`
	External       bool      `json:"external,omitempty"`
	AckClosedAt    time.Time `json:"ack_closed_at,omitempty"`
	AckUpdatedAt   time.Time `json:"ack_updated_at,omitempty"`
	AckHead        string    `json:"ack_head,omitempty"`
	AcknowledgedBy string    `json:"acknowledged_by,omitempty"`
	// EvidenceURL documents operational validation for the delivery at EvidenceHead.
	// EvidenceSource is "pr" (found in the merged PR) or "operator" (reconcile --evidence).
	EvidenceURL    string `json:"evidence_url,omitempty"`
	EvidenceHead   string `json:"evidence_head,omitempty"`
	EvidenceSource string `json:"evidence_source,omitempty"`
}
type ChildStatus struct {
	Title     string `json:"title,omitempty"`
	Issue     int    `json:"issue"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	PR        int    `json:"pr,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
}
type Supervisor struct {
	RetryAt        time.Time             `json:"retry_at,omitempty"`
	RemoteFailures int                   `json:"remote_failures,omitempty"`
	WorkpadRetryAt time.Time             `json:"workpad_retry_at,omitempty"`
	ID             string                `json:"id"`
	ProjectID      string                `json:"project_id"`
	Parent         Issue                 `json:"parent"`
	Plan           SupervisorPlan        `json:"plan"`
	Revision       string                `json:"revision"`
	Trigger        string                `json:"trigger"`
	State          string                `json:"state"`
	Reason         string                `json:"reason,omitempty"`
	Hold           string                `json:"hold,omitempty"`
	Current        int                   `json:"current,omitempty"`
	Ready          []int                 `json:"ready,omitempty"`
	Evidence       map[int]ChildEvidence `json:"evidence"`
	Children       []ChildStatus         `json:"children"`
	CommentID      int64                 `json:"comment_id,omitempty"`
	PublishedHash  string                `json:"published_hash,omitempty"`
	WorkpadError   string                `json:"workpad_error,omitempty"`
	Updated        time.Time             `json:"updated"`
}

func (i Issue) Supervisor() bool { return i.HasLabel("supervisor") }
func (i Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

const planFence = "```changathi-supervisor"

// HasPlanBlock reports whether body uses the deprecated fenced plan.
func HasPlanBlock(body string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == planFence {
			return true
		}
	}
	return false
}

// ParseSupervisor reads the deprecated fenced YAML plan from a parent body.
func ParseSupervisor(body string, parent int) (SupervisorPlan, string, error) {
	var plan SupervisorPlan
	var blocks []string
	var lines []string
	inside := false
	// Only exact fences carry executable plan data; prose cannot add dependencies.
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == planFence {
			if inside {
				return plan, "", fmt.Errorf("nested supervisor fence")
			}
			inside = true
			lines = nil
			continue
		}
		if inside && t == "```" {
			blocks = append(blocks, strings.Join(lines, "\n"))
			inside = false
			continue
		}
		if inside {
			lines = append(lines, line)
		}
	}
	if inside || len(blocks) != 1 {
		return plan, "", fmt.Errorf("exactly one complete changathi-supervisor block is required")
	}
	dec := yaml.NewDecoder(strings.NewReader(blocks[0]))
	dec.KnownFields(true)
	if err := dec.Decode(&plan); err != nil {
		return plan, "", err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return plan, "", fmt.Errorf("only one YAML document is allowed")
	}
	plan.Source = PlanBody
	return validatePlan(plan, parent)
}

// NativePlan builds a plan from the parent's sub-issues (in GitHub's order) and
// each child's blocked-by issues. Every blocker must itself be a sub-issue of the
// parent, so the graph never silently depends on issues outside the plan.
func NativePlan(repo string, parent int, subs []Issue, blockedBy map[int][]Issue) (SupervisorPlan, string, error) {
	plan := SupervisorPlan{Version: 1, Source: PlanSubIssues}
	if len(subs) == 0 {
		return plan, "", fmt.Errorf("add the child issues as sub-issues of #%d", parent)
	}
	siblings := map[int64]int{}
	for _, i := range subs {
		if !i.InRepo(repo) {
			return plan, "", fmt.Errorf("sub-issue #%d belongs to another repository", i.Number)
		}
		if i.PullRequest != nil || i.Supervisor() {
			return plan, "", fmt.Errorf("sub-issue #%d must be a normal issue without the supervisor label", i.Number)
		}
		siblings[i.ID] = i.Number
	}
	for _, i := range subs {
		c := SupervisorChild{Issue: i.Number, DependsOn: []int{}, Completion: "merged_and_closed", RequiresEvidence: i.HasLabel(EvidenceLabel)}
		if i.HasLabel(ManualLabel) {
			c.Completion = "closed_completed"
		}
		for _, b := range blockedBy[i.Number] {
			n, ok := siblings[b.ID]
			if !ok {
				return plan, "", fmt.Errorf("#%d is blocked by #%d, which is not a sub-issue of #%d; add it as a sub-issue or remove the dependency", i.Number, b.Number, parent)
			}
			c.DependsOn = append(c.DependsOn, n)
		}
		sort.Ints(c.DependsOn)
		plan.Children = append(plan.Children, c)
	}
	return validatePlan(plan, parent)
}

// InRepo reports whether the issue's API repository URL names repo.
func (i Issue) InRepo(repo string) bool {
	return i.RepositoryURL == "" || strings.EqualFold(strings.TrimSuffix(i.RepositoryURL, "/"), "https://api.github.com/repos/"+repo)
}

func validatePlan(plan SupervisorPlan, parent int) (SupervisorPlan, string, error) {
	if plan.Version != 1 || len(plan.Children) == 0 || len(plan.Children) > 100 {
		return plan, "", fmt.Errorf("version 1 requires 1–100 children")
	}
	byNumber := map[int]SupervisorChild{}
	edges := 0
	for idx := range plan.Children {
		c := &plan.Children[idx]
		if c.Completion == "" {
			c.Completion = "merged_and_closed"
		}
		if c.Completion != "merged_and_closed" && c.Completion != "closed_completed" {
			return plan, "", fmt.Errorf("#%d: invalid completion policy", c.Issue)
		}
		if c.Issue < 1 || c.Issue == parent {
			return plan, "", fmt.Errorf("invalid child #%d", c.Issue)
		}
		if _, exists := byNumber[c.Issue]; exists {
			return plan, "", fmt.Errorf("duplicate child #%d", c.Issue)
		}
		if c.DependsOn == nil {
			c.DependsOn = []int{}
		}
		byNumber[c.Issue] = *c
		edges += len(c.DependsOn)
	}
	if edges > 500 {
		return plan, "", fmt.Errorf("maximum 500 dependency edges")
	}
	for _, c := range plan.Children {
		seen := map[int]bool{}
		for _, dep := range c.DependsOn {
			if _, ok := byNumber[dep]; !ok || dep == c.Issue || seen[dep] {
				return plan, "", fmt.Errorf("#%d: invalid or duplicate dependency #%d", c.Issue, dep)
			}
			seen[dep] = true
		}
	}
	visiting := map[int]int{}
	var visit func(int) error
	visit = func(n int) error {
		if visiting[n] == 1 {
			return fmt.Errorf("dependency cycle at #%d", n)
		}
		if visiting[n] == 2 {
			return nil
		}
		visiting[n] = 1
		for _, d := range byNumber[n].DependsOn {
			if err := visit(d); err != nil {
				return err
			}
		}
		visiting[n] = 2
		return nil
	}
	for n := range byNumber {
		if err := visit(n); err != nil {
			return plan, "", err
		}
	}
	return plan, SupervisorHash(plan), nil
}
func SupervisorHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (s Supervisor) Child(n int) (SupervisorChild, bool) {
	for _, c := range s.Plan.Children {
		if c.Issue == n {
			return c, true
		}
	}
	return SupervisorChild{}, false
}

// Selected reports whether the latest reconciliation chose child n for dispatch.
func (v Supervisor) Selected(n int) bool {
	for _, x := range v.Ready {
		if x == n {
			return true
		}
	}
	return false
}
