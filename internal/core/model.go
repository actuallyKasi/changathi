package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func ID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func Now() time.Time { return time.Now().UTC() }

var NamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type Config struct {
	DataDir     string `json:"data_dir" yaml:"data_dir"`
	Concurrency int    `json:"concurrency" yaml:"concurrency"`
	PollSeconds int    `json:"poll_seconds" yaml:"poll_seconds"`
	Codex       string `json:"codex" yaml:"codex"`
	MaxLogBytes int64  `json:"max_log_bytes" yaml:"max_log_bytes"`
	EventLimit  int    `json:"event_limit" yaml:"event_limit"`
}

func DefaultConfig() Config {
	h, _ := os.UserHomeDir()
	return Config{DataDir: filepath.Join(h, ".local", "share", "changathi"), Concurrency: 2, PollSeconds: 30, Codex: "codex", MaxLogBytes: 16 * 1024 * 1024, EventLimit: 10000}
}
func (c Config) Socket() string { return filepath.Join(c.DataDir, "daemon.sock") }
func (c Config) Validate() error {
	if c.Concurrency < 1 || c.PollSeconds < 1 || c.Codex == "" {
		return fmt.Errorf("concurrency, poll_seconds and codex must be valid")
	}
	if !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("data directory must be absolute")
	}
	if c.MaxLogBytes < 65536 || c.EventLimit < 100 {
		return fmt.Errorf("max_log_bytes must be at least 65536 and event_limit at least 100")
	}
	return nil
}

type Project struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Repo        string `json:"repo"`
	RepoID      int64  `json:"repo_id"`
	Base        string `json:"base"`
	CloneURL    string `json:"clone_url"`
	Enabled     bool   `json:"enabled"`
	Auto        bool   `json:"auto"`
	Concurrency int    `json:"concurrency"`
	Error       string `json:"error,omitempty"`
}
type Issue struct {
	ID          int64     `json:"id"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	StateReason string    `json:"state_reason"`
	ClosedAt    time.Time `json:"closed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	HTMLURL     string    `json:"html_url"`
	Labels      []Label   `json:"labels"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
}
type Label struct {
	Name string `json:"name"`
}

func (i Issue) Ready() bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l.Name, "agent:ready") {
			return true
		}
	}
	return false
}

type Attempt struct {
	SupervisorID string    `json:"supervisor_id,omitempty"`
	PlanRevision string    `json:"plan_revision,omitempty"`
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	Issue        Issue     `json:"issue"`
	Generation   int       `json:"generation"`
	Trigger      string    `json:"trigger"`
	State        string    `json:"state"`
	Error        string    `json:"error,omitempty"`
	Branch       string    `json:"branch"`
	Workspace    string    `json:"workspace,omitempty"`
	Thread       string    `json:"thread,omitempty"`
	Turn         string    `json:"turn,omitempty"`
	PR           int       `json:"pr,omitempty"`
	Head         string    `json:"head,omitempty"`
	BaseSHA      string    `json:"base_sha,omitempty"`
	Workflow     Workflow  `json:"workflow"`
	Retries      int       `json:"retries"`
	Turns        int       `json:"turns"`
	Due          time.Time `json:"due,omitempty"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
	PID          int       `json:"pid,omitempty"`
	ReadyRemoved bool      `json:"ready_removed,omitempty"`
	Activity     string    `json:"activity,omitempty"`
}

func (a Attempt) Active() bool {
	switch a.State {
	case "done", "superseded", "cancelled", "needs_attention", "awaiting_review":
		return false
	}
	return true
}
func (a Attempt) Runnable() bool {
	switch a.State {
	case "queued", "retry_wait", "waiting_account", "waiting_checks":
		return !a.Due.After(Now())
	}
	return false
}

type Message struct {
	ID        string    `json:"id"`
	AttemptID string    `json:"attempt_id"`
	Text      string    `json:"text"`
	State     string    `json:"state"`
	Created   time.Time `json:"created"`
	Error     string    `json:"error,omitempty"`
}
type Event struct {
	Version   int       `json:"version,omitempty"`
	ProjectID string    `json:"project_id,omitempty"`
	ThreadID  string    `json:"thread_id,omitempty"`
	TurnID    string    `json:"turn_id,omitempty"`
	Output    *Output   `json:"output,omitempty"`
	Seq       int64     `json:"seq"`
	AttemptID string    `json:"attempt_id,omitempty"`
	Type      string    `json:"type"`
	Text      string    `json:"text"`
	Created   time.Time `json:"created"`
}

// Output is the provider-neutral transcript payload. Snapshot replaces accumulated
// deltas for the same item; it is never appended to them.
type Output struct {
	Kind       string `json:"kind"`
	ItemID     string `json:"item_id"`
	Text       string `json:"text,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Status     string `json:"status,omitempty"`
	Title      string `json:"title,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}
type Operation struct {
	ID        string `json:"id"`
	AttemptID string `json:"attempt_id"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Result    string `json:"result,omitempty"`
}
type Snapshot struct {
	Supervisors  []Supervisor  `json:"supervisors"`
	Projects     []Project     `json:"projects"`
	Agents       []Attempt     `json:"agents"`
	Messages     []Message     `json:"messages"`
	Events       []Event       `json:"events,omitempty"`
	Usage        any           `json:"usage,omitempty"`
	ProviderWait *ProviderGate `json:"provider_wait,omitempty"`
}

type ProviderGate struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}
