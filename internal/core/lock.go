package core

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

type executionFileKey struct{}

// The locked open-file description is inherited by workers. A daemon crash cannot
// release its claim while a child process still has the descriptor open.
func ExecutionLock(dir, id string) (*os.File, error) {
	root := filepath.Join(dir, "workers")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("previous execution still holds its process lock: %w", err)
	}
	return f, nil
}
func WithExecutionLock(ctx context.Context, f *os.File) context.Context {
	return context.WithValue(ctx, executionFileKey{}, f)
}
func InheritedFiles(ctx context.Context) []*os.File {
	if f, ok := ctx.Value(executionFileKey{}).(*os.File); ok {
		return []*os.File{f}
	}
	return nil
}
