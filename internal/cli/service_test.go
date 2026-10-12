package cli

import (
	"bytes"
	"changathi/internal/core"
	"changathi/internal/service"
	"changathi/packaging"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderLaunchdEscapesPaths(t *testing.T) {
	out, err := renderService(packaging.Launchd, serviceUnit{Args: []string{"/Users/a&b/bin/changathi", "daemon", "start"}, Path: "/Users/a&b/bin:/usr/bin", LogDir: "/Users/a&b/Library/Logs"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<array><string>/Users/a&amp;b/bin/changathi</string><string>daemon</string><string>start</string></array>",
		"<key>PATH</key><string>/Users/a&amp;b/bin:/usr/bin</string>",
		"<string>/Users/a&amp;b/Library/Logs/changathi.error.log</string>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "/Users/you") {
		t.Fatal("template placeholder left in output")
	}
}

func TestRenderSystemdQuotesArguments(t *testing.T) {
	out, err := renderService(packaging.Systemd, serviceUnit{Args: []string{"/home/a b/changathi", "daemon", "start", "--config", "/home/a/100%.yaml"}, Path: "/home/a b/bin:/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ExecStart="/home/a b/changathi" daemon start --config /home/a/100%%.yaml`,
		`Environment="PATH=/home/a b/bin:/usr/bin"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}

func TestServicePathOrdersAndDedupes(t *testing.T) {
	got := servicePath([]string{"/opt/tools/bin", "/usr/bin", "/opt/tools/bin/"}, "relative:/home/me/go/bin:/usr/bin", "darwin")
	want := "/opt/tools/bin:/usr/bin:/home/me/go/bin:/opt/homebrew/bin:/usr/local/bin:/bin:/usr/sbin:/sbin"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// TestDaemonInstallIsIdempotent drives install against a fake service
// manager that runs the daemon in-process.
func TestDaemonInstallIsIdempotent(t *testing.T) {
	for _, tool := range []string{"git", "gh", "codex"} {
		if _, err := exec.LookPath(tool); err != nil {
			fake := t.TempDir()
			for _, name := range []string{"gh", "codex"} {
				if err := os.WriteFile(filepath.Join(fake, name), []byte("#!/bin/sh\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
			break
		}
	}
	dir, err := os.MkdirTemp("/tmp", "changathi-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "unit", "changathi.service")
	loads := 0
	var stop context.CancelFunc
	var done chan error
	running := func() bool { return stop != nil }
	defer func() {
		if running() {
			stop()
			<-done
		}
	}()
	old := serviceManagerFor
	defer func() { serviceManagerFor = old }()
	serviceManagerFor = func() (serviceManager, error) {
		return serviceManager{file: file, template: packaging.Systemd, logs: dir,
			active: func(context.Context) bool { return running() },
			load: func(_ context.Context, _ bool) error {
				loads++
				if running() {
					stop()
					<-done
				}
				cfg := core.DefaultConfig()
				cfg.DataDir = filepath.Join(dir, "data")
				var ctx context.Context
				ctx, stop = context.WithCancel(context.Background())
				done = make(chan error, 1)
				go func() { done <- service.Serve(ctx, cfg) }()
				return nil
			},
			unload: func(context.Context) error { stop(); <-done; stop = nil; return nil },
		}, nil
	}
	run := func(args ...string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		cmd := New()
		cmd.SetOut(&out)
		cmd.SetArgs(append([]string{"--json", "--data-dir", filepath.Join(dir, "data")}, args...))
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(out.Bytes(), &v); err != nil {
			t.Fatalf("%v: %s", err, out.String())
		}
		return v
	}
	first := run("daemon", "install")
	if first["updated"] != true || loads != 1 {
		t.Fatalf("first install: %v, loads %d", first, loads)
	}
	unit, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(unit), "daemon start --data-dir "+filepath.Join(dir, "data")) {
		t.Fatalf("unit: %v\n%s", err, unit)
	}
	second := run("daemon", "install")
	if second["updated"] != false || second["restarted"] != false || loads != 1 {
		t.Fatalf("second install: %v, loads %d", second, loads)
	}
	removed := run("daemon", "uninstall")
	if removed["removed"] != true || running() {
		t.Fatalf("uninstall: %v", removed)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("unit file left behind")
	}
}
