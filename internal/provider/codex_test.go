package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A real subprocess speaking the protocol catches framing, correlation, and exit issues.
func TestRPCProcess(t *testing.T) {
	if os.Getenv("CHANGATHI_RPC_HELPER") != "1" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		var v struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &v) != nil {
			os.Exit(3)
		}
		reply := func(result any) { _ = enc.Encode(map[string]any{"id": v.ID, "result": result}) }
		switch v.Method {
		case "initialize":
			reply(map[string]any{"userAgent": "fake"})
		case "initialized":
		case "thread/start", "thread/resume":
			reply(map[string]any{"thread": map[string]string{"id": "thread1"}})
		case "turn/start":
			reply(map[string]any{"turn": map[string]string{"id": "turn1"}})
			_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread1", "turn": map[string]string{"id": "old-turn", "status": "failed"}}})
			_ = enc.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"threadId": "thread1", "turnId": "turn1", "delta": "hello"}})
			_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread1", "turn": map[string]string{"id": "turn1", "status": "completed"}}})
		case "turn/interrupt":
			reply(map[string]any{})
		default: // deliberately leave unknown requests unanswered for cancellation testing
		}
	}
	os.Exit(0)
}
func fakeExecutable(t *testing.T) string {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "fake-codex")
	script := fmt.Sprintf("#!/bin/sh\nCHANGATHI_RPC_HELPER=1 exec '%s' -test.run=TestRPCProcess -- \"$@\"\n", strings.ReplaceAll(exe, "'", "'\\''"))
	if e = os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return path
}
func TestCodexProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, e := (Codex{Command: fakeExecutable(t)}).Open(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	if e = session.Start(ctx, Request{Workspace: t.TempDir(), Thread: "thread1"}); e != nil {
		t.Fatal(e)
	}
	events := []Event{}
	if e = session.Run(ctx, "Implement feature", func(v Event) { events = append(events, v) }); e != nil {
		t.Fatal(e)
	}
	found := false
	for _, v := range events {
		if v.Text == "hello" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing streamed message")
	}
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if _, e = session.(*codexSession).call(short, "unanswered", nil); e == nil {
		t.Fatal("request did not time out")
	}
}
func TestInstalledCodexHandshake(t *testing.T) {
	if os.Getenv("CHANGATHI_CODEX_SMOKE") != "1" {
		t.Skip("opt-in installed Codex smoke check; no inference")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, e := (Codex{Command: "codex"}).Open(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	if e = session.Start(ctx, Request{Workspace: t.TempDir()}); e != nil {
		t.Fatal(e)
	}
}
