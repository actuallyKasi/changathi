package provider

import (
	"changathi/internal/core"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a CLI stand-in that records its arguments and first stdin
// line, replays a recorded stream-json fixture, then waits for stdin to close
// like the real CLI does between turns.
func fakeClaude(t *testing.T, fixture string) (command, dir string) {
	t.Helper()
	dir = t.TempDir()
	src, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > '%[1]s/args'\nIFS= read -r line\nprintf '%%s\\n' \"$line\" > '%[1]s/input'\ncat '%[2]s'\ncat > /dev/null\n", dir, src)
	command = filepath.Join(dir, "claude")
	if err = os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return command, dir
}

func runClaude(t *testing.T, fixture string, r Request) ([]Event, string, error) {
	t.Helper()
	command, dir := fakeClaude(t, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := (Claude{Command: command}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r.Workspace = t.TempDir()
	if err = s.Start(ctx, r); err != nil {
		t.Fatal(err)
	}
	var events []Event
	err = s.Run(ctx, "Implement feature", func(v Event) { events = append(events, v) })
	_ = s.Close()
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	input, _ := os.ReadFile(filepath.Join(dir, "input"))
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(input, &msg) != nil || msg.Type != "user" || msg.Message.Content[0].Text != "Implement feature" {
		t.Fatalf("stdin message %q", input)
	}
	return events, string(args), err
}

func TestClaudeImplementTurn(t *testing.T) {
	events, args, err := runClaude(t, "testdata/claude-implement.jsonl", Request{Model: "claude-opus-5-5", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--input-format\nstream-json", "--model\nclaude-opus-5-5", "--effort\nhigh", "--permission-mode\nbypassPermissions", "--permission-prompts\nnone", "--session-id"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in args:\n%s", want, args)
		}
	}
	if events[0].Type != "session_started" || events[0].Thread == "" || events[0].PID == 0 {
		t.Fatalf("first event %+v", events[0])
	}
	kinds := map[string]*core.Output{}
	for _, v := range events {
		if v.Output != nil {
			kinds[v.Output.Kind+"/"+v.Output.Mode] = v.Output
		}
	}
	if o := kinds["tool/start"]; o == nil || o.Title != "Bash: echo probe-ok" {
		t.Fatalf("tool start %+v", o)
	}
	if o := kinds["tool/snapshot"]; o == nil || !strings.Contains(o.Text, "probe-ok") || o.Status != "completed" {
		t.Fatalf("tool result %+v", o)
	}
	if o := kinds["assistant/snapshot"]; o == nil || o.Text != "done" {
		t.Fatalf("assistant %+v", o)
	}
	if o := kinds["turn/snapshot"]; o == nil || o.Status != "completed" {
		t.Fatalf("turn %+v", o)
	}
	var usage bool
	for _, v := range events {
		if v.Type == "claude/usage" && strings.Contains(v.Text, `"model":"claude-opus-5-5"`) && strings.Contains(v.Text, "cost_usd") {
			usage = true
		}
	}
	if !usage {
		t.Fatal("missing usage event")
	}
}

func TestClaudeVerifierPostureAndStructuredOutput(t *testing.T) {
	events, args, err := runClaude(t, "testdata/claude-verify.jsonl", Request{Model: "claude-sonnet-5-5", ReadOnly: true, Schema: core.VerdictSchema})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--permission-mode\ndontAsk", "--setting-sources\nuser", "--no-session-persistence", "--tools\nRead,Grep,Glob,Bash", "--json-schema"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in args:\n%s", want, args)
		}
	}
	if strings.Contains(args, "bypassPermissions") || strings.Contains(args, "--resume") {
		t.Fatalf("verifier args too permissive:\n%s", args)
	}
	var final string
	for _, v := range events {
		if v.Type == "claude/result" {
			final = v.Text
		}
	}
	if v, err := core.ParseVerdict(final); err != nil || v.Verdict != "pass" {
		t.Fatalf("verdict %q: %v", final, err)
	}
}

func TestClaudeResumeAndFailures(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, lines ...string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	limited := write("limited.jsonl",
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day_opus","resetsAt":1792170000}}`,
		`{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"limit"}]},"error":"rate_limit"}`,
		`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit"}`)
	_, args, err := runClaude(t, limited, Request{Model: "claude-opus-5-5", Thread: "11111111-1111-4111-8111-111111111111"})
	if err == nil || !strings.Contains(err.Error(), "usage limit (seven_day_opus)") {
		t.Fatalf("limit error %v", err)
	}
	var limit *LimitError
	if !errors.As(err, &limit) || !limit.Resets.Equal(time.Unix(1792170000, 0)) {
		t.Fatalf("limit reset %v", err)
	}
	if !strings.Contains(args, "--resume\n11111111-1111-4111-8111-111111111111") || strings.Contains(args, "--session-id") {
		t.Fatalf("resume args:\n%s", args)
	}
	auth := write("auth.jsonl",
		`{"type":"assistant","message":{"id":"m","content":[]},"error":"authentication_failed"}`,
		`{"type":"result","subtype":"success","is_error":true,"result":"OAuth session expired"}`)
	if _, _, err = runClaude(t, auth, Request{Model: "claude-opus-5-5"}); err == nil || !strings.Contains(err.Error(), "claude auth login") {
		t.Fatalf("auth error %v", err)
	}
	exited := write("exit.jsonl", `{"type":"system","subtype":"init","session_id":"x"}`)
	command, _ := fakeClaude(t, exited)
	// Replace the waiting tail so the process exits before any result.
	b, _ := os.ReadFile(command)
	_ = os.WriteFile(command, []byte(strings.Replace(string(b), "cat > /dev/null\n", "echo boom >&2\n", 1)), 0700)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, _ := (Claude{Command: command}).Open(ctx)
	defer s.Close()
	if err = s.Start(ctx, Request{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err = s.Run(ctx, "Implement feature", func(Event) {}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("exit error %v", err)
	}
}

// The CLI reports a missing session only on stderr, after an error result.
func TestClaudeLostSession(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "lost.jsonl")
	_ = os.WriteFile(fixture, []byte(`{"type":"result","subtype":"error_during_execution","is_error":true}`+"\n"), 0600)
	run := func(thread string) error {
		command, _ := fakeClaude(t, fixture)
		b, _ := os.ReadFile(command)
		_ = os.WriteFile(command, []byte(strings.Replace(string(b), "cat > /dev/null\n", "echo 'No conversation found with session ID: x' >&2\n", 1)), 0700)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, _ := (Claude{Command: command}).Open(ctx)
		defer s.Close()
		if err := s.Start(ctx, Request{Workspace: t.TempDir(), Thread: thread}); err != nil {
			t.Fatal(err)
		}
		return s.Run(ctx, "Implement feature", func(Event) {})
	}
	if err := run("11111111-1111-4111-8111-111111111111"); !errors.Is(err, ErrSessionLost) {
		t.Fatalf("resume error %v", err)
	}
	if err := run(""); err == nil || errors.Is(err, ErrSessionLost) {
		t.Fatalf("a new session cannot be lost: %v", err)
	}
}

// Opt-in live check against the installed, logged-in CLI. Spends one small Sonnet turn.
func TestInstalledClaudeTurn(t *testing.T) {
	if os.Getenv("CHANGATHI_CLAUDE_SMOKE") != "1" {
		t.Skip("set CHANGATHI_CLAUDE_SMOKE=1 to run one live Claude turn")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := (Claude{Command: "claude"}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Start(ctx, Request{Workspace: t.TempDir(), Model: "claude-sonnet-5-5", Effort: "low", ReadOnly: true, Schema: core.VerdictSchema}); err != nil {
		t.Fatal(err)
	}
	var final string
	err = s.Run(ctx, `There is no change to review. Return {"verdict":"pass","findings":[]}.`, func(v Event) {
		if v.Type == "claude/result" {
			final = v.Text
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := core.ParseVerdict(final); err != nil || v.Verdict != "pass" {
		t.Fatalf("verdict %q: %v", final, err)
	}
	// Resuming a session the CLI never saved must surface as ErrSessionLost.
	lost, err := (Claude{Command: "claude"}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lost.Close()
	if err = lost.Start(ctx, Request{Workspace: t.TempDir(), Model: "claude-sonnet-5-5", Thread: newUUID()}); err != nil {
		t.Fatal(err)
	}
	if err = lost.Run(ctx, "reply ok", func(Event) {}); !errors.Is(err, ErrSessionLost) {
		t.Fatalf("lost session error %v", err)
	}
}
