package cli

import (
	"changathi/internal/core"
	"changathi/packaging"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
)

const launchdLabel = "dev.changathi"

// serviceUnit is what the launchd and systemd templates are filled in with.
type serviceUnit struct {
	Args   []string
	Path   string
	LogDir string
}

// serviceManager wraps the OS service manager that supervises the daemon.
type serviceManager struct {
	file     string
	template string
	logs     string
	active   func(context.Context) bool
	load     func(ctx context.Context, changed bool) error
	unload   func(context.Context) error
}

func (a *app) daemonInstall() *cobra.Command {
	var print bool
	cmd := &cobra.Command{Use: "install", Short: "Write the launchd agent (macOS) or systemd user unit (Linux) and start the daemon; safe to re-run", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		m, err := serviceManagerFor()
		if err != nil {
			return err
		}
		unit, err := a.serviceUnit(c, m.logs)
		if err != nil {
			return err
		}
		content, err := renderService(m.template, unit)
		if err != nil {
			return err
		}
		if print {
			_, err = fmt.Fprint(a.out, content)
			return err
		}
		ctx := c.Context()
		old, err := os.ReadFile(m.file)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		changed := string(old) != content
		active := m.active(ctx)
		if !active {
			if _, err := a.client().Status(ctx); err == nil {
				return fmt.Errorf("a daemon is already running outside the service; stop it with `changathi daemon stop` and re-run install")
			}
		}
		if changed {
			if err := writeFileAtomic(m.file, content); err != nil {
				return err
			}
		}
		if changed || !active {
			if err := m.load(ctx, changed); err != nil {
				return err
			}
		}
		if err := a.waitForDaemon(ctx, 15*time.Second); err != nil {
			return fmt.Errorf("service loaded but the daemon is not answering (%v); see %s", err, m.logs)
		}
		return a.display(map[string]any{"service": m.file, "updated": changed, "restarted": changed || !active, "binary": unit.Args[0], "path": unit.Path, "logs": m.logs, "daemon": "running"})
	}}
	cmd.Flags().BoolVar(&print, "print", false, "Print the service file instead of installing it")
	return cmd
}

func (a *app) daemonUninstall() *cobra.Command {
	return &cobra.Command{Use: "uninstall", Short: "Stop the service and remove its launchd agent or systemd unit; state is kept", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		m, err := serviceManagerFor()
		if err != nil {
			return err
		}
		if m.active(c.Context()) {
			if err := m.unload(c.Context()); err != nil {
				return err
			}
		}
		removed := true
		if err := os.Remove(m.file); os.IsNotExist(err) {
			removed = false
		} else if err != nil {
			return err
		}
		return a.display(map[string]any{"service": m.file, "removed": removed, "data_dir": a.config.DataDir})
	}}
}

// serviceManagerFor is replaced in tests.
var serviceManagerFor = newServiceManager

func newServiceManager() (serviceManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceManager{}, err
	}
	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		target := domain + "/" + launchdLabel
		file := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		loaded := func(ctx context.Context) bool {
			_, err := core.Command(ctx, "", "launchctl", "print", target)
			return err == nil
		}
		return serviceManager{
			file: file, template: packaging.Launchd, logs: filepath.Join(home, "Library", "Logs"),
			active: loaded,
			load: func(ctx context.Context, _ bool) error {
				if loaded(ctx) {
					_, _ = core.Command(ctx, "", "launchctl", "bootout", target)
					for i := 0; i < 50 && loaded(ctx); i++ {
						time.Sleep(100 * time.Millisecond)
					}
				}
				// Bootstrap can briefly fail with an I/O error right after a bootout.
				var err error
				for i := 0; i < 5; i++ {
					if _, err = core.Command(ctx, "", "launchctl", "bootstrap", domain, file); err == nil {
						return nil
					}
					time.Sleep(500 * time.Millisecond)
				}
				return err
			},
			unload: func(ctx context.Context) error {
				_, err := core.Command(ctx, "", "launchctl", "bootout", target)
				return err
			},
		}, nil
	case "linux":
		systemctl := func(ctx context.Context, args ...string) error {
			_, err := core.Command(ctx, "", "systemctl", append([]string{"--user"}, args...)...)
			return err
		}
		return serviceManager{
			file: filepath.Join(home, ".config", "systemd", "user", "changathi.service"), template: packaging.Systemd, logs: "journalctl --user -u changathi",
			active: func(ctx context.Context) bool { return systemctl(ctx, "is-active", "--quiet", "changathi") == nil },
			load: func(ctx context.Context, changed bool) error {
				if err := systemctl(ctx, "daemon-reload"); err != nil {
					return err
				}
				if err := systemctl(ctx, "enable", "changathi"); err != nil {
					return err
				}
				if changed {
					return systemctl(ctx, "restart", "changathi")
				}
				return systemctl(ctx, "start", "changathi")
			},
			unload: func(ctx context.Context) error {
				if err := systemctl(ctx, "disable", "--now", "changathi"); err != nil {
					return err
				}
				return systemctl(ctx, "daemon-reload")
			},
		}, nil
	}
	return serviceManager{}, fmt.Errorf("daemon install supports macOS and Linux, not %s", runtime.GOOS)
}

// serviceUnit resolves the binary and tool paths the service needs. The
// service's PATH puts the tools changathi runs first, then the installing
// shell's PATH so validation commands find the same compilers as a terminal.
func (a *app) serviceUnit(c *cobra.Command, logDir string) (serviceUnit, error) {
	bin, err := installedBinary()
	if err != nil {
		return serviceUnit{}, err
	}
	dirs := []string{filepath.Dir(bin)}
	for _, tool := range []string{"git", "gh", a.config.Codex} {
		p, err := exec.LookPath(tool)
		if err != nil {
			return serviceUnit{}, fmt.Errorf("%s not found on PATH; install it or run `changathi doctor`", tool)
		}
		dirs = append(dirs, filepath.Dir(p))
	}
	// The npm build of codex is a node script.
	if p, err := exec.LookPath("node"); err == nil {
		dirs = append(dirs, filepath.Dir(p))
	}
	args := []string{bin, "daemon", "start"}
	if a.configFile != "" {
		file, err := filepath.Abs(a.configFile)
		if err != nil {
			return serviceUnit{}, err
		}
		args = append(args, "--config", file)
	}
	if c.Flags().Changed("data-dir") {
		args = append(args, "--data-dir", a.config.DataDir)
	}
	return serviceUnit{Args: args, Path: servicePath(dirs, os.Getenv("PATH"), runtime.GOOS), LogDir: logDir}, nil
}

// installedBinary prefers the changathi found on PATH (often a symlink into
// the checkout, which keeps working across rebuilds) when it is this binary.
func installedBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := exec.LookPath("changathi"); err == nil {
		if p, err = filepath.Abs(p); err == nil && sameFile(p, self) {
			return p, nil
		}
	}
	return self, nil
}

func sameFile(a, b string) bool {
	x, err := os.Stat(a)
	if err != nil {
		return false
	}
	y, err := os.Stat(b)
	return err == nil && os.SameFile(x, y)
}

func servicePath(dirs []string, current, goos string) string {
	defaults := []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	if goos == "darwin" {
		defaults = append([]string{"/opt/homebrew/bin"}, defaults...)
	}
	all := append(append(dirs, filepath.SplitList(current)...), defaults...)
	seen := map[string]bool{}
	out := []string{}
	for _, d := range all {
		d = filepath.Clean(d)
		if !filepath.IsAbs(d) || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

func renderService(text string, unit serviceUnit) (string, error) {
	t, err := template.New("service").Funcs(template.FuncMap{"systemdQuote": systemdQuote, "systemdArgs": func(args []string) string {
		quoted := make([]string, len(args))
		for i, v := range args {
			quoted[i] = systemdQuote(strings.ReplaceAll(v, "$", "$$"))
		}
		return strings.Join(quoted, " ")
	}}).Parse(text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, unit); err != nil {
		return "", err
	}
	return b.String(), nil
}

// systemdQuote escapes a value for an ExecStart= or Environment= line.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if s != "" && !strings.ContainsAny(s, " \t\"'\\;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func writeFileAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(content); err == nil {
		err = tmp.Chmod(0644)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (a *app) waitForDaemon(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := a.client().Status(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
