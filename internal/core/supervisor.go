package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
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
}
type ChildEvidence struct {
	ApprovalCommentID int64     `json:"approval_comment_id,omitempty"`
	IssueID           int64     `json:"issue_id"`
	PR                int       `json:"pr,omitempty"`
	External          bool      `json:"external,omitempty"`
	AckClosedAt       time.Time `json:"ack_closed_at,omitempty"`
	AckUpdatedAt      time.Time `json:"ack_updated_at,omitempty"`
	AckHead           string    `json:"ack_head,omitempty"`
	EvidenceURL       string    `json:"evidence_url,omitempty"`
	AcknowledgedBy    string    `json:"acknowledged_by,omitempty"`
}
type ChildStatus struct {
	Title     string `json:"title,omitempty"`
	Issue     int    `json:"issue"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	PR        int    `json:"pr,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
}
type ApprovalPrompt struct {
	PR  int    `json:"pr"`
	URL string `json:"url"`
}
type Supervisor struct {
	ApprovalPrompts map[int]ApprovalPrompt `json:"approval_prompts,omitempty"`
	RetryAt         time.Time              `json:"retry_at,omitempty"`
	RemoteFailures  int                    `json:"remote_failures,omitempty"`
	WorkpadRetryAt  time.Time              `json:"workpad_retry_at,omitempty"`
	ID              string                 `json:"id"`
	ProjectID       string                 `json:"project_id"`
	Parent          Issue                  `json:"parent"`
	Plan            SupervisorPlan         `json:"plan"`
	Revision        string                 `json:"revision"`
	Trigger         string                 `json:"trigger"`
	State           string                 `json:"state"`
	Reason          string                 `json:"reason,omitempty"`
	Hold            string                 `json:"hold,omitempty"`
	Current         int                    `json:"current,omitempty"`
	Evidence        map[int]ChildEvidence  `json:"evidence"`
	Children        []ChildStatus          `json:"children"`
	CommentID       int64                  `json:"comment_id,omitempty"`
	PublishedHash   string                 `json:"published_hash,omitempty"`
	WorkpadError    string                 `json:"workpad_error,omitempty"`
	Updated         time.Time              `json:"updated"`
}

func (i Issue) Supervisor() bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l.Name, "supervisor") {
			return true
		}
	}
	return false
}
func ParseSupervisor(body string, parent int) (SupervisorPlan, string, error) {
	var plan SupervisorPlan
	var blocks []string
	var lines []string
	inside := false
	// Only exact fences carry executable plan data; prose cannot add dependencies.
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "```changathi-supervisor" {
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
