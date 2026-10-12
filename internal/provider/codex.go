package provider

import (
	"bufio"
	"changathi/internal/core"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Codex struct{ Command string }
type rpc struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}
type codexSession struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	write   sync.Mutex
	mu      sync.Mutex
	pending map[string]chan rpc
	events  chan rpc
	done    chan struct{}
	seq     atomic.Int64
	thread  string
	closed  atomic.Bool
	err     error
}

func (s *codexSession) ProcessID() int { return s.cmd.Process.Pid }

func (c Codex) Open(ctx context.Context) (Session, error) {
	cmd := exec.CommandContext(ctx, c.Command, "app-server", "--listen", "stdio://")
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
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	stderr, e := cmd.StderrPipe()
	if e != nil {
		return nil, e
	}
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	s := &codexSession{cmd: cmd, stdin: in, pending: map[string]chan rpc{}, events: make(chan rpc, 4096), done: make(chan struct{})}
	go io.Copy(io.Discard, stderr)
	go s.read(out)
	go func() { e := cmd.Wait(); s.mu.Lock(); s.err = e; s.mu.Unlock(); close(s.done) }()
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, e = s.call(initCtx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "changathi", "title": "Changathi", "version": "0.1.0"}, "capabilities": map[string]bool{"experimentalApi": false}}); e != nil {
		s.Close()
		return nil, e
	}
	e = s.send(map[string]any{"method": "initialized"})
	if e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}
func (s *codexSession) send(v any) error {
	s.write.Lock()
	defer s.write.Unlock()
	return json.NewEncoder(s.stdin).Encode(v)
}
func (s *codexSession) read(r io.Reader) {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 65536), 16*1024*1024)
	for scan.Scan() {
		var v rpc
		if json.Unmarshal(scan.Bytes(), &v) != nil {
			continue
		}
		if len(v.ID) > 0 && v.Method == "" {
			s.mu.Lock()
			ch := s.pending[string(v.ID)]
			s.mu.Unlock()
			if ch != nil {
				ch <- v
			}
			continue
		}
		if len(v.ID) > 0 { // unattended runner never waits indefinitely for unsupported client tools
			_ = s.send(map[string]any{"id": v.ID, "error": map[string]any{"code": -32601, "message": "Changathi does not support interactive client requests; use available workspace tools"}})
			continue
		}
		select {
		case s.events <- v:
		default: // abort instead of silently losing turn-completed events
			s.mu.Lock()
			s.err = errors.New("Codex event buffer exhausted")
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
func (s *codexSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.seq.Add(1)
	key := fmt.Sprint(id)
	ch := make(chan rpc, 1)
	s.mu.Lock()
	s.pending[key] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, key); s.mu.Unlock() }()
	if e := s.send(map[string]any{"id": id, "method": method, "params": params}); e != nil {
		return nil, e
	}
	select {
	case v := <-ch:
		if v.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, v.Error.Message)
		}
		return v.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		s.mu.Lock()
		e := s.err
		s.mu.Unlock()
		return nil, fmt.Errorf("Codex exited: %v", e)
	}
}
func (s *codexSession) Start(ctx context.Context, r Request) error {
	params := map[string]any{"cwd": r.Workspace, "approvalPolicy": "never", "sandbox": "danger-full-access"}
	if r.ReadOnly {
		params["sandbox"] = "read-only"
	}
	if r.Model != "" {
		params["model"] = r.Model
	}
	method := "thread/start"
	if r.Thread != "" {
		method = "thread/resume"
		params["threadId"] = r.Thread
	}
	b, e := s.call(ctx, method, params)
	if e != nil {
		return e
	}
	var v struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return e
	}
	if v.Thread.ID == "" {
		return errors.New("Codex returned empty thread ID")
	}
	s.thread = v.Thread.ID
	return nil
}
func input(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}
}
func (s *codexSession) Run(ctx context.Context, prompt string, emit func(Event)) error {
	b, e := s.call(ctx, "turn/start", map[string]any{"threadId": s.thread, "input": input(prompt)})
	if e != nil {
		return e
	}
	var start struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if e = json.Unmarshal(b, &start); e != nil {
		return e
	}
	emit(Event{Type: "session_started", Thread: s.thread, Turn: start.Turn.ID, PID: s.cmd.Process.Pid})
	for {
		select {
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = s.call(stop, "turn/interrupt", map[string]string{"threadId": s.thread, "turnId": start.Turn.ID})
			cancel()
			return ctx.Err()
		case <-s.done:
			return errors.New("Codex exited before turn completion")
		case v := <-s.events:
			var p struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Delta    string `json:"delta"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Error  *struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(v.Params, &p)
			if p.ThreadID != "" && p.ThreadID != s.thread {
				continue
			}
			if p.TurnID != "" && p.TurnID != start.Turn.ID {
				continue
			}
			if p.Turn.ID != "" && p.Turn.ID != start.Turn.ID {
				continue
			}
			text := p.Delta
			if text == "" {
				text = string(v.Params)
			}
			emit(Event{Type: v.Method, Text: text, Turn: start.Turn.ID, Output: NormalizeOutput(v.Method, v.Params, start.Turn.ID)})
			if v.Method == "turn/completed" && p.Turn.ID == start.Turn.ID {
				if p.Turn.Status != "completed" {
					if p.Turn.Error != nil {
						return fmt.Errorf("Codex turn failed: %s", p.Turn.Error.Message)
					}
					return fmt.Errorf("Codex turn %s", p.Turn.Status)
				}
				return nil
			}
		}
	}
}
func (s *codexSession) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.stdin.Close()
	select {
	case <-s.done:
		return nil
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
		return nil
	}
}

// Usage reads account metadata and reported limits without starting an inference turn.
func (c Codex) Usage(ctx context.Context) (any, error) {
	session, err := c.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	s := session.(*codexSession)
	account, err := s.call(ctx, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return nil, err
	}
	limits, err := s.call(ctx, "account/rateLimits/read", map[string]any{})
	if err != nil {
		return nil, err
	}
	return map[string]json.RawMessage{"account": account, "limits": limits}, nil
}
