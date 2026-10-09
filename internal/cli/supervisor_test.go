package cli

import (
	"bytes"
	"changathi/internal/core"
	"changathi/internal/service"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestSupervisorCLIRequests(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "changathi-super-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "daemon.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan service.Request, 10)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req service.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requests <- req
		_ = json.NewEncoder(w).Encode(service.Response{Data: map[string]bool{"ok": true}})
	})}
	go srv.Serve(listener)
	defer srv.Close()
	cases := []struct {
		args       []string
		action     string
		apply, ack bool
		child      int
	}{
		{[]string{"start", "homelab#12"}, "supervisor.start", false, false, 0},
		{[]string{"reload", "homelab#12"}, "supervisor.reload", false, false, 0},
		{[]string{"reload", "homelab#12", "--apply"}, "supervisor.reload", true, false, 0},
		{[]string{"reconcile", "homelab#12", "--child", "4", "--ack-completion", "--evidence", "https://example.test/report"}, "supervisor.reconcile", false, true, 4},
	}
	for _, tc := range cases {
		a := &app{config: core.DefaultConfig(), out: &bytes.Buffer{}}
		a.config.DataDir = dir
		cmd := a.supervisor()
		cmd.SetArgs(tc.args)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		r := <-requests
		if r.Project != "homelab" || r.Number != 12 || r.Action != tc.action || r.Apply != tc.apply || r.AckCompletion != tc.ack || r.Child != tc.child {
			t.Fatal(r)
		}
	}
}
