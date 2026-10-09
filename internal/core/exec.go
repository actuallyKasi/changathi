package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func Command(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.ExtraFiles = InheritedFiles(ctx)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	b, e := cmd.CombinedOutput()
	if e != nil {
		return string(b), fmt.Errorf("%s: %w: %s", name, e, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}
func PrivateEnvironment() []string {
	out := []string{}
	for _, v := range os.Environ() {
		key := strings.SplitN(v, "=", 2)[0]
		switch key {
		case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP", "LANG", "TZ", "TERM", "CODEX_HOME", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
			out = append(out, v)
		default:
			if strings.HasPrefix(key, "LC_") {
				out = append(out, v)
			}
		}
	}
	return out
}
