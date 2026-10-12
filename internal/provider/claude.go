package provider

import (
	"bufio"
	"changathi/internal/core"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrSessionLost reports that a resumed session no longer exists, for example
// because the CLI exited before saving it or its local history was deleted.
var ErrSessionLost = errors.New("provider session could not be resumed")

// Claude drives the Claude Code CLI in print mode with stream-json on stdin and
// stdout. One process serves one session; each Run writes one user message and
// reads until that turn's result event.
type Claude struct{ Command string }

// verifierTools is the read-only tool surface for a verify session.
var verifierTools = []string{"Read", "Grep", "Glob", "Bash(git diff *)", "Bash(git log *)", "Bash(git show *)", "Bash(git status *)"}

type claudeLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Model     string          `json:"model"`
	Error     string          `json:"error"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	Duration  int64           `json:"duration_ms"`
	CostUSD   float64         `json:"total_cost_usd"`
	Usage     json.RawMessage `json:"usage"`
	Structure json.RawMessage `json:"structured_output"`
	RateLimit struct {
		Status string `json:"status"`
		Type   string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
	Message struct {
		ID      string `json:"id"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		} `json:"content"`
	} `json:"message"`
	raw []byte
}

type claudeSession struct {
	command string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	write   sync.Mutex
	lines   chan claudeLine
	done    chan struct{}
	mu      sync.Mutex
	err     error
	stderr  strings.Builder
	session string
	resumed bool
	model   string
	turns   atomic.Int64
	closed  atomic.Bool
}

func (c Claude) Open(context.Context) (Session, error) {
	return &claudeSession{command: c.Command, lines: make(chan claudeLine, 4096), done: make(chan struct{})}, nil
}

// ProcessID is zero until Start launches the CLI; the engine also records the PID from events.
func (s *claudeSession) ProcessID() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Args builds the CLI invocation for a request. Exported for tests and diagnostics.
func (c Claude) Args(r Request, session string) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none"}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.Effort != "" {
		args = append(args, "--effort", r.Effort)
	}
	if r.ReadOnly {
		// Verifiers never resume, load no project settings or hooks from the issue
		// branch, and may run only the pre-approved read-only tools.
		args = append(args, "--permission-mode", "dontAsk", "--setting-sources", "user", "--no-session-persistence",
			"--tools", "Read,Grep,Glob,Bash", "--allowedTools", strings.Join(verifierTools, ","))
	} else {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	if r.Schema != "" {
		args = append(args, "--json-schema", r.Schema)
	}
	if r.Thread != "" {
		args = append(args, "--resume", r.Thread)
	} else {
		args = append(args, "--session-id", session)
	}
	return args
}

func (s *claudeSession) Start(ctx context.Context, r Request) error {
	if s.cmd != nil {
		return errors.New("Claude session already started")
	}
	s.session, s.resumed = r.Thread, r.Thread != ""
	if s.session == "" {
		s.session = newUUID()
	}
	s.model = r.Model
	cmd := exec.CommandContext(ctx, s.command, Claude{}.Args(r, s.session)...)
	cmd.Dir = r.Workspace
	cmd.Env = core.PrivateEnvironment()
	cmd.ExtraFiles = core.InheritedFiles(ctx)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	in, e := cmd.StdinPipe()
	if e != nil {
		return e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return e
	}
	stderr, e := cmd.StderrPipe()
	if e != nil {
		return e
	}
	if e = cmd.Start(); e != nil {
		return e
	}
	s.cmd, s.stdin = cmd, in
	go s.captureStderr(stderr)
	go s.read(out)
	go func() { e := cmd.Wait(); s.mu.Lock(); s.err = e; s.mu.Unlock(); close(s.done) }()
	return nil
}

func (s *claudeSession) captureStderr(r io.Reader) {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 65536), 1024*1024)
	for scan.Scan() {
		s.mu.Lock()
		if s.stderr.Len() < 8192 {
			s.stderr.WriteString(scan.Text())
			s.stderr.WriteString("\n")
		}
		s.mu.Unlock()
	}
}

func (s *claudeSession) read(r io.Reader) {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 65536), 16*1024*1024)
	for scan.Scan() {
		var v claudeLine
		if json.Unmarshal(scan.Bytes(), &v) != nil {
			continue
		}
		v.raw = append([]byte(nil), scan.Bytes()...)
		select {
		case s.lines <- v:
		default: // abort instead of silently losing a turn's result event
			s.mu.Lock()
			s.err = errors.New("Claude event buffer exhausted")
			s.mu.Unlock()
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			return
		}
	}
	if e := scan.Err(); e != nil {
		s.mu.Lock()
		s.err = e
		s.mu.Unlock()
	}
}

func (s *claudeSession) send(text string) error {
	s.write.Lock()
	defer s.write.Unlock()
	return json.NewEncoder(s.stdin).Encode(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}},
	})
}

// sessionLost reports whether a failed resume failed because the session is gone.
// The CLI says so only on stderr, which can trail the result event, so wait
// briefly for the process to exit before deciding.
func (s *claudeSession) sessionLost() bool {
	if !s.resumed {
		return false
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
	}
	time.Sleep(50 * time.Millisecond) // let the stderr reader drain
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.stderr.String(), "No conversation found with session ID")
}

func (s *claudeSession) exitError() error {
	if s.sessionLost() {
		return fmt.Errorf("%w: %s", ErrSessionLost, s.session)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	detail := strings.TrimSpace(s.stderr.String())
	if detail == "" && s.err != nil {
		detail = s.err.Error()
	}
	return fmt.Errorf("Claude exited before the turn completed: %s", detail)
}

func (s *claudeSession) Run(ctx context.Context, prompt string, emit func(Event)) error {
	if s.cmd == nil {
		return errors.New("Claude session not started")
	}
	turn := fmt.Sprintf("%s-%d", shortID(s.session), s.turns.Add(1))
	emit(Event{Type: "session_started", Thread: s.session, Turn: turn, PID: s.ProcessID()})
	if e := s.send(prompt); e != nil {
		return e
	}
	titles := map[string]string{}
	var failure string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			// Drain lines that arrived before exit; the result may already be buffered.
			for {
				select {
				case v := <-s.lines:
					if done, err := s.handle(v, turn, titles, &failure, emit); done {
						return err
					}
				default:
					return s.exitError()
				}
			}
		case v := <-s.lines:
			if done, err := s.handle(v, turn, titles, &failure, emit); done {
				return err
			}
		}
	}
}

// handle emits one line's normalized events and reports whether the turn ended.
func (s *claudeSession) handle(v claudeLine, turn string, titles map[string]string, failure *string, emit func(Event)) (bool, error) {
	raw := string(v.raw)
	switch v.Type {
	case "assistant":
		// Keep the more specific rate-limit window when the generic error follows it.
		if v.Error != "" && !strings.HasPrefix(*failure, v.Error) {
			*failure = v.Error
		}
		for i, c := range v.Message.Content {
			switch c.Type {
			case "text":
				if c.Text == "" {
					continue
				}
				emit(Event{Type: "claude/assistant", Text: c.Text, Turn: turn, Output: &core.Output{Kind: "assistant", ItemID: fmt.Sprintf("%s-%d", v.Message.ID, i), Text: c.Text, Mode: "snapshot", Status: "completed"}})
			case "tool_use":
				title := toolTitle(c.Name, c.Input)
				titles[c.ID] = title
				emit(Event{Type: "claude/tool_use", Text: raw, Turn: turn, Output: &core.Output{Kind: "tool", ItemID: c.ID, Title: title, Mode: "start", Status: "running"}})
			}
		}
	case "user":
		for _, c := range v.Message.Content {
			if c.Type != "tool_result" {
				continue
			}
			status := "completed"
			if c.IsError {
				status = "failed"
			}
			text := claudeToolText(c.Content)
			emit(Event{Type: "claude/tool_result", Text: text, Turn: turn, Output: &core.Output{Kind: "tool", ItemID: c.ToolUseID, Title: titles[c.ToolUseID], Text: text, Mode: "snapshot", Status: status}})
		}
	case "result":
		status := "completed"
		var err error
		if v.IsError || v.Subtype != "success" {
			status = "failed"
			err = claudeError(*failure, v.Subtype, v.Result)
			if v.Subtype == "error_during_execution" && s.sessionLost() {
				err = fmt.Errorf("%w: %s", ErrSessionLost, s.session)
			}
		}
		usage, _ := json.Marshal(map[string]any{"model": s.model, "cost_usd": v.CostUSD, "usage": v.Usage})
		emit(Event{Type: "claude/usage", Text: string(usage), Turn: turn})
		final := v.Result
		if len(v.Structure) > 0 && string(v.Structure) != "null" {
			final = string(v.Structure)
		}
		text := ""
		if err != nil {
			text = err.Error()
		}
		emit(Event{Type: "claude/result", Text: final, Turn: turn, Output: &core.Output{Kind: "turn", ItemID: "turn-" + turn, Mode: "snapshot", Status: status, Text: text, DurationMS: v.Duration}})
		return true, err
	case "rate_limit_event":
		if v.RateLimit.Status == "rejected" {
			*failure = "rate_limit:" + v.RateLimit.Type
		}
		emit(Event{Type: "claude/rate_limit", Text: raw, Turn: turn})
	}
	return false, nil
}

// claudeError maps CLI failures onto the engine's account-limit and attention paths.
func claudeError(failure, subtype, result string) error {
	detail := strings.TrimSpace(result)
	if detail == "" {
		detail = subtype
	}
	kind, window, _ := strings.Cut(failure, ":")
	switch kind {
	case "rate_limit", "billing_error":
		if window != "" {
			return fmt.Errorf("Claude usage limit (%s): %s", window, detail)
		}
		return fmt.Errorf("Claude usage limit: %s", detail)
	case "authentication_failed":
		return fmt.Errorf("Claude authentication failed (run `claude auth login`): %s", detail)
	}
	return fmt.Errorf("Claude turn failed: %s", detail)
}

func toolTitle(name string, input json.RawMessage) string {
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Pattern  string `json:"pattern"`
	}
	_ = json.Unmarshal(input, &in)
	for _, detail := range []string{in.Command, in.FilePath, in.Pattern} {
		if detail != "" {
			return name + ": " + detail
		}
	}
	return name
}

func claudeToolText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return contentText(raw)
}

func (s *claudeSession) Close() error {
	if s.closed.Swap(true) || s.cmd == nil {
		return nil
	}
	s.stdin.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
	return nil
}

// Probe runs one minimal, tool-free turn to check that a model is reachable.
func (c Claude) Probe(ctx context.Context, dir, model string) (string, error) {
	args := []string{"-p", "--output-format", "json", "--model", model, "--tools", "", "--no-session-persistence", "--setting-sources", "user", "Reply with the single word ok."}
	cmd := exec.CommandContext(ctx, c.Command, args...)
	cmd.Dir = dir
	cmd.Env = core.PrivateEnvironment()
	out, err := cmd.Output()
	var v claudeLine
	if json.Unmarshal(out, &v) == nil && v.Type == "result" {
		if v.IsError {
			return "", claudeError("", v.Subtype, v.Result)
		}
		return "reachable", nil
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return "", err
	}
	return "", errors.New("unexpected probe output")
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ClaudeTestedVersion is the Claude Code release the adapter's fixtures were recorded with.
const ClaudeTestedVersion = "2.1.296"
