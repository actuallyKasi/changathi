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
}
type Session interface {
	Start(context.Context, Request) error
	Run(context.Context, string, func(Event)) error
	Steer(context.Context, string, string) error
	Close() error
}
type Provider interface {
	Open(context.Context) (Session, error)
}
