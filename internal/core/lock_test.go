package core

import (
	"context"
	"os/exec"
	"testing"
)

func TestWorkerKeepsLockAfterParentClosesDescriptor(t *testing.T) {
	dir := t.TempDir()
	id := ID()
	guard, err := ExecutionLock(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithExecutionLock(context.Background(), guard)
	cmd := exec.Command("sh", "-c", "sleep 1")
	cmd.ExtraFiles = InheritedFiles(ctx)
	if err = cmd.Start(); err != nil {
		guard.Close()
		t.Fatal(err)
	}
	// Start establishes that the inherited descriptor has been duplicated before
	// simulating the parent's descriptor disappearing in a crash.
	guard.Close()
	if next, e := ExecutionLock(dir, id); e == nil {
		next.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("lock released while inherited worker was running")
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	next, err := ExecutionLock(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
