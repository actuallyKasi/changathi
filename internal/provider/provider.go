package provider

import (
	"changathi/internal/core"
	"context"
)

type Event struct {
	Output *core.Output
	Type   string
	Text   string
	Thread string
	Turn   string
	PID    int
}
type Request struct {
	Workspace string
	Thread    string
	Model     string
	Prompt    string
	Effort    string
	// ReadOnly selects the verifier posture: no edits, no resume, read-only tools.
	ReadOnly bool
	// Schema requests structured output validated against this JSON Schema.
	Schema string
}
type Session interface {
	Start(context.Context, Request) error
	Run(context.Context, string, func(Event)) error
	Close() error
}
type Provider interface {
	Open(context.Context) (Session, error)
}
