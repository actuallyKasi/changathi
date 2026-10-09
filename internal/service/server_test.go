package service

import (
	"changathi/internal/core"
	"context"
	"testing"
)

func TestRequestIdempotency(t *testing.T) {
	e, _, ps := setup(t, 1, &fakeProvider{})
	s := &Server{Engine: e}
	r := Request{ID: core.ID(), Action: "issue.run", Project: ps[0].Name, Number: 1}
	first, err := s.action(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.action(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || second == nil {
		t.Fatal("missing response")
	}
	as, _ := core.List[core.Attempt](e.Store, "attempt")
	if len(as) != 1 {
		t.Fatal("duplicate dispatch")
	}
	r = Request{ID: core.ID(), Action: "issue.retry", Project: ps[0].Name, Number: 1}
	_ = e.Store.Put("request_intent", r.ID, core.Operation{ID: r.ID, State: "intent"})
	if _, err = s.action(context.Background(), r); err == nil {
		t.Fatal("replayed uncertain external mutation")
	}
}
