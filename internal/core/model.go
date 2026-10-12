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
	Claude      string `json:"claude" yaml:"claude"`
	MaxLogBytes int64  `json:"max_log_bytes" yaml:"max_log_bytes"`
	EventLimit  int    `json:"event_limit" yaml:"event_limit"`
	// Providers is the host allow-list; repository workflows cannot widen it.
	// Nil keeps the original behavior: Codex only, any model.
	Providers map[string]ProviderPolicy `json:"providers,omitempty" yaml:"providers"`
}

// ProviderPolicy limits the models a provider may run. Empty Models permits any model.
type ProviderPolicy struct {
	Models []string `json:"models" yaml:"models"`
}

func DefaultConfig() Config {
	h, _ := os.UserHomeDir()
	return Config{DataDir: filepath.Join(h, ".local", "share", "changathi"), Concurrency: 1, PollSeconds: 30, Codex: "codex", Claude: "claude", MaxLogBytes: 16 * 1024 * 1024, EventLimit: 10000}
}

// Allows reports whether the host permits a workflow role's provider and model.
func (c Config) Allows(r Role) error {
	if c.Providers == nil {
		if r.Provider != "codex" {
			return fmt.Errorf("provider %s is not enabled in the service configuration", r.Provider)
		}
		return nil
	}
	policy, ok := c.Providers[r.Provider]
	if !ok {
		return fmt.Errorf("provider %s is not enabled in the service configuration", r.Provider)
	}
	if len(policy.Models) == 0 {
		return nil
	}
	for _, m := range policy.Models {
		if m == r.Model {
			return nil
		}
	}
	return fmt.Errorf("model %q is not allowed for provider %s", r.Model, r.Provider)
}
func (c Config) Socket() string { return filepath.Join(c.DataDir, "daemon.sock") }
func (c Config) Validate() error {
	if c.Concurrency < 1 || c.PollSeconds < 1 || c.Codex == "" || c.Claude == "" {
		return fmt.Errorf("concurrency, poll_seconds, codex and claude must be valid")
	}
	for name := range c.Providers {
		if !KnownProvider(name) {
			return fmt.Errorf("unknown provider %q in providers", name)
		}
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
	SupervisorID string `json:"supervisor_id,omitempty"`
	PlanRevision string `json:"plan_revision,omitempty"`
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	Issue        Issue  `json:"issue"`
	Generation   int    `json:"generation"`
	Trigger      string `json:"trigger"`
	State        string `json:"state"`
	Error        string `json:"error,omitempty"`
	Branch       string `json:"branch"`
	Workspace    string `json:"workspace,omitempty"`
	Thread       string `json:"thread,omitempty"`
	// ThreadProvider names the provider that owns Thread; a thread is never resumed by another provider.
	ThreadProvider string         `json:"thread_provider,omitempty"`
	Verifications  []Verification `json:"verifications,omitempty"`
	// VerifyFailures counts failing verdicts since the last explicit retry.
	VerifyFailures int `json:"verify_failures,omitempty"`
	// LastFailure is the latest failure the agent can act on (validation output,
	// blocking findings, a failing check). Operational waits never overwrite it,
	// unlike Error, which is the operator-facing reason for the current state.
	LastFailure string `json:"last_failure,omitempty"`
	// NextStep is "validate" once an implement turn has completed, so re-entry
	// after an interruption or a verifier wait resumes without another turn.
	NextStep string `json:"next_step,omitempty"`
	// Feedback holds the operator's issue comments captured when rework was
	// requested; FeedbackAt is the newest comment time already captured, so a
	// later rework only passes newer comments.
	Feedback   string    `json:"feedback,omitempty"`
	FeedbackAt time.Time `json:"feedback_at,omitempty"`
	// Reply is the agent's answer to the operator's comments when a rework turn
	// ended with no code change; Changathi posts it on the issue.
	Reply string `json:"reply,omitempty"`
	// StatusPosted is a digest of the status comment last published on the issue.
	StatusPosted string `json:"status_posted,omitempty"`
	// Previous summarizes the generation this one reworks.
	Previous     *PreviousGeneration `json:"previous,omitempty"`
	Turn         string              `json:"turn,omitempty"`
	PR           int                 `json:"pr,omitempty"`
	Head         string              `json:"head,omitempty"`
	BaseSHA      string              `json:"base_sha,omitempty"`
	Workflow     Workflow            `json:"workflow"`
	Retries      int                 `json:"retries"`
	Turns        int                 `json:"turns"`
	Due          time.Time           `json:"due,omitempty"`
	Created      time.Time           `json:"created"`
	Updated      time.Time           `json:"updated"`
	PID          int                 `json:"pid,omitempty"`
	ReadyRemoved bool                `json:"ready_removed,omitempty"`
	Activity     string              `json:"activity,omitempty"`
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
	Supervisors  []Supervisor   `json:"supervisors"`
	Projects     []Project      `json:"projects"`
	Agents       []Attempt      `json:"agents"`
	Events       []Event        `json:"events,omitempty"`
	Usage        map[string]any `json:"usage,omitempty"`
	ProviderWait *ProviderGate  `json:"provider_wait,omitempty"`
	// ProviderWaits lists every active gate; ProviderWait is the earliest-ending one.
	ProviderWaits []ProviderGate `json:"provider_waits,omitempty"`
}

type ProviderGate struct {
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	Until    time.Time `json:"until"`
	Reason   string    `json:"reason"`
}

// GateKey identifies an account gate. A model-specific limit is keyed by provider and model.
func GateKey(provider, model string) string {
	if model == "" {
		return provider
	}
	return provider + "/" + model
}
