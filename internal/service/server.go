package service

import (
	"changathi/internal/core"
	"changathi/internal/provider"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Request struct {
	Child         int    `json:"child,omitempty"`
	PR            int    `json:"pr,omitempty"`
	AckCompletion bool   `json:"ack_completion,omitempty"`
	Evidence      string `json:"evidence,omitempty"`
	ID            string `json:"id"`
	Action        string `json:"action"`
	Project       string `json:"project,omitempty"`
	Attempt       string `json:"attempt,omitempty"`
	Number        int    `json:"number,omitempty"`
	Text          string `json:"text,omitempty"`
	Repo          string `json:"repo,omitempty"`
	Auto          bool   `json:"auto,omitempty"`
	Concurrency   int    `json:"concurrency,omitempty"`
	Apply         bool   `json:"apply,omitempty"`
}
type Response struct {
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}
type Server struct {
	Engine  *Engine
	cancel  context.CancelFunc
	actions sync.Mutex
}

func Serve(ctx context.Context, c core.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(c.DataDir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(c.DataDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("another daemon owns this data directory: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	s, err := core.OpenStore(c.DataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	s.EventLimit = c.EventLimit
	_ = os.Remove(c.Socket())
	listener, err := net.Listen("unix", c.Socket())
	if err != nil {
		return err
	}
	defer os.Remove(c.Socket())
	if err = os.Chmod(c.Socket(), 0600); err != nil {
		listener.Close()
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	engine := New(c, s, core.GH{}, provider.Codex{Command: c.Codex})
	server := &Server{Engine: engine, cancel: cancel}
	httpServer := &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx) }()
	go func() {
		<-ctx.Done()
		stop, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = httpServer.Shutdown(stop)
	}()
	fmt.Fprintf(os.Stderr, "Changathi daemon ready (PID %d). Socket: %s\nRunning in the foreground; Ctrl-C stops the daemon. Use another terminal for commands.\n", os.Getpid(), c.Socket())
	err = httpServer.Serve(listener)
	cancel()
	engineErr := <-done
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return engineErr
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var data any
	var err error
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/status":
		data, err = s.Engine.Store.Snapshot()
	case r.Method == "GET" && r.URL.Path == "/v1/events":
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		data, err = s.Engine.Store.Events(after, 500)
	case r.Method == "GET" && r.URL.Path == "/v1/transcript":
		q := r.URL.Query()
		var after, before int64
		var limit int
		after, err = strconv.ParseInt(defaultQuery(q.Get("after"), "0"), 10, 64)
		if err == nil {
			before, err = strconv.ParseInt(defaultQuery(q.Get("before"), "0"), 10, 64)
		}
		if err == nil {
			limit, err = strconv.Atoi(defaultQuery(q.Get("limit"), "200"))
		}
		if err == nil {
			data, err = s.Engine.Store.EventPage(q.Get("attempt"), after, before, limit)
		}
	case r.Method == "POST" && r.URL.Path == "/v1/action":
		var req Request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
		dec.DisallowUnknownFields()
		err = dec.Decode(&req)
		if err == nil {
			data, err = s.action(r.Context(), req)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(Response{Error: "unknown API route"})
		return
	}
	out := Response{Data: data}
	if err != nil {
		out.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func defaultQuery(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func (s *Server) action(ctx context.Context, r Request) (any, error) {
	if r.Action == "issue.list" || r.Action == "issue.show" || r.Action == "usage" || r.Action == "supervisor.show" || r.Action == "supervisor.validate" {
		return s.perform(ctx, r)
	}
	if len(r.ID) != 32 {
		return nil, errors.New("mutation requires a 32-character request ID")
	}
	s.actions.Lock()
	defer s.actions.Unlock()
	var cached Response
	if s.Engine.Store.Get("request", r.ID, &cached) == nil {
		if cached.Error != "" {
			return nil, errors.New(cached.Error)
		}
		return cached.Data, nil
	}
	var marker core.Operation
	if s.Engine.Store.Get("request_intent", r.ID, &marker) == nil {
		return nil, errors.New("previous request outcome is uncertain; inspect remote state before resubmitting")
	}
	if err := s.Engine.Store.Put("request_intent", r.ID, core.Operation{ID: r.ID, Kind: r.Action, State: "intent"}); err != nil {
		return nil, err
	}
	data, err := s.perform(ctx, r)
	response := Response{Data: data}
	if err != nil {
		response.Error = err.Error()
	}
	if save := s.Engine.Store.Put("request", r.ID, response); save != nil && err == nil {
		return nil, save
	}
	return data, err
}
func (s *Server) perform(ctx context.Context, r Request) (any, error) {
	e := s.Engine
	switch r.Action {
	case "usage":
		p, ok := e.Provider.(interface {
			Usage(context.Context) (any, error)
		})
		if !ok {
			return nil, errors.New("provider does not expose usage metadata")
		}
		value, err := p.Usage(ctx)
		if err == nil {
			err = e.Store.Put("usage", "codex", value)
		}
		return value, err
	case "daemon.stop":
		s.cancel()
		return "stopping", nil
	case "project.add":
		if !core.NamePattern.MatchString(r.Project) {
			return nil, errors.New("project name must contain only letters, numbers, hyphens or underscores")
		}
		p, err := e.GitHub.Repository(ctx, r.Repo)
		if err != nil {
			return nil, err
		}
		p.Name = r.Project
		p.Auto = r.Auto
		if r.Concurrency > 0 {
			p.Concurrency = r.Concurrency
		}
		warnings, err := e.GitHub.Prepare(ctx, p)
		if err != nil {
			return nil, err
		}
		if err = e.Store.AddProject(p); err != nil {
			return nil, err
		}
		e.Wake()
		return map[string]any{"project": p, "warnings": warnings}, nil
	case "project.pause", "project.resume":
		p, err := e.Store.Project(r.Project)
		if err != nil {
			return nil, err
		}
		lock := e.lock(p.ID)
		lock.Lock()
		defer lock.Unlock()
		err = e.Store.UpdateProject(p.ID, func(x *core.Project) error { x.Enabled = r.Action == "project.resume"; p = *x; return nil })
		e.Wake()
		return p, err
	case "agent.message":
		return e.Message(ctx, r.Attempt, r.Text)
	case "agent.stop":
		return "cancelled", e.Cancel(r.Attempt)
	case "workspace.clean":
		a, err := e.current(r.Attempt)
		if err != nil {
			return nil, err
		}
		p, err := e.Store.Project(a.ProjectID)
		if err != nil {
			return nil, err
		}
		lock := e.lock(p.ID)
		lock.Lock()
		defer lock.Unlock()
		err = e.Workspaces.Clean(ctx, p, a, r.Apply)
		return map[string]any{"path": a.Workspace, "removed": r.Apply && err == nil}, err
	}
	p, err := e.Store.Project(r.Project)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(r.Action, "supervisor.") {
		return e.supervisorAction(ctx, p, r)
	}
	switch r.Action {
	case "issue.list":
		return e.GitHub.Issues(ctx, p, false)
	case "issue.show":
		return e.GitHub.Issue(ctx, p, r.Number)
	case "issue.run", "issue.retry":
		if !p.Enabled {
			return nil, errors.New("project is paused")
		}
		i, err := e.GitHub.Issue(ctx, p, r.Number)
		if err != nil {
			return nil, err
		}
		if i.State != "open" {
			return nil, errors.New("issue must be open")
		}
		if r.Action == "issue.retry" {
			as, _ := core.List[core.Attempt](e.Store, "attempt")
			e.mu.Lock()
			for _, old := range as {
				if old.ProjectID == p.ID && old.Issue.ID == i.ID {
					if _, active := e.workers[old.ID]; active {
						e.mu.Unlock()
						return nil, errors.New("worker is still stopping/running; retry after it exits")
					}
				}
			}
			e.mu.Unlock()
		}
		a, err := e.enqueueIssue(ctx, p, i, r.Action == "issue.retry")
		e.Wake()
		return a, err
	case "issue.cancel":
		as, err := core.List[core.Attempt](e.Store, "attempt")
		if err != nil {
			return nil, err
		}
		for j := len(as) - 1; j >= 0; j-- {
			if as[j].ProjectID == p.ID && as[j].Issue.Number == r.Number && as[j].Active() {
				return "cancelled", e.Cancel(as[j].ID)
			}
		}
		return nil, errors.New("no active attempt")
	default:
		return nil, fmt.Errorf("unknown action %q", r.Action)
	}
}
