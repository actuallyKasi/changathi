package cli

import (
	"changathi/internal/core"
	"changathi/internal/service"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUnixDaemonLifecycle(t *testing.T) {
	dir, e := os.MkdirTemp("/tmp", "changathi-test-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	cfg := core.DefaultConfig()
	cfg.DataDir = dir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Serve(ctx, cfg) }()
	client := NewClient(cfg.Socket())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e = client.Status(ctx); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, e := os.Stat(cfg.Socket())
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket permissions")
	}
	if e = service.Serve(ctx, cfg); e == nil {
		t.Fatal("duplicate daemon accepted")
	}
	if e = client.Action(ctx, service.Request{Action: "daemon.stop"}, nil); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if _, e = os.Stat(filepath.Join(dir, "daemon.sock")); !os.IsNotExist(e) {
		t.Fatal("socket not removed")
	}
}
