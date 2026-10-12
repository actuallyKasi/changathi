package core

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentClaimsAndRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := Project{ID: ID(), Name: "one", RepoID: 1}
	if err = s.AddProject(p); err != nil {
		t.Fatal(err)
	}
	issue := Issue{ID: 42, Number: 1, State: "open"}
	var wg sync.WaitGroup
	for j := 0; j < 30; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Enqueue(p, issue, "manual", false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	as, _ := List[Attempt](s, "attempt")
	if len(as) != 1 {
		t.Fatalf("got %d claims", len(as))
	}
	a := as[0]
	_ = s.UpdateAttempt(a.ID, func(a *Attempt) error {
		a.State = "needs_attention"
		a.Workspace = filepath.Join(dir, "work")
		a.Thread = "thread"
		a.Workflow = Workflow{Revision: "old"}
		return nil
	})
	retried, err := s.Enqueue(p, issue, "manual", true)
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != a.ID || retried.Thread != "thread" {
		t.Fatal("retry lost session/worktree")
	}
	if retried.Workflow.Revision != "" {
		t.Fatal("retry kept the stale workflow snapshot")
	}
	_ = s.UpdateAttempt(a.ID, func(a *Attempt) error { a.State = "awaiting_review"; return nil })
	same, _ := s.Enqueue(p, issue, "auto", false)
	if same.ID != a.ID {
		t.Fatal("redispatched review item")
	}
	next, _ := s.Enqueue(p, issue, "manual", true)
	if next.Generation != 2 || next.ID == a.ID {
		t.Fatal("rework needs fresh generation")
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	as, _ = List[Attempt](s, "attempt")
	if len(as) != 2 {
		t.Fatal("state not durable")
	}
	same, _ = s.Enqueue(p, issue, "auto", false)
	if same.ID != next.ID {
		t.Fatal("did not return latest active generation")
	}
}
func TestRepoIdentityAndEventCursor(t *testing.T) {
	s, e := OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p := Project{ID: ID(), Name: "a", RepoID: 1}
	q := Project{ID: ID(), Name: "b", RepoID: 2}
	_ = s.AddProject(p)
	_ = s.AddProject(q)
	if e = s.AddProject(Project{ID: ID(), Name: "c", RepoID: 1}); e == nil {
		t.Fatal("duplicate repository accepted")
	}
	a, _ := s.Enqueue(p, Issue{ID: 10, Number: 1}, "manual", false)
	b, _ := s.Enqueue(q, Issue{ID: 11, Number: 1}, "manual", false)
	if a.ID == b.ID || a.Branch == b.Branch {
		t.Fatal("cross-project collision")
	}
	_ = s.Event(a.ID, "test", "one")
	_ = s.Event(b.ID, "test", "two")
	xs, e := s.Events(0, 1)
	if e != nil || len(xs) != 1 {
		t.Fatal(e)
	}
	ys, e := s.Events(xs[0].Seq, 100)
	if e != nil || len(ys) != 1 || ys[0].Text != "two" {
		t.Fatal("cursor failed")
	}
}

func TestExpiredEventCursor(t *testing.T) {
	s, e := OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.EventLimit = 100
	for n := 0; n < 200; n++ {
		if e = s.Event("", "test", "event"); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.Events(1, 100); e == nil {
		t.Fatal("expired cursor silently skipped events")
	}
	events, e := s.Events(0, 500)
	if e != nil || len(events) != 100 {
		t.Fatalf("retention %d %v", len(events), e)
	}
}

func TestRetrySupersedesReviewAndRepairsHistoricalRows(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := Project{ID: ID(), Name: "test", RepoID: 1}
	i := Issue{ID: 42, Number: 1, State: "open"}
	a, err := s.Enqueue(p, i, "manual", false)
	if err != nil {
		t.Fatal(err)
	}
	s.UpdateAttempt(a.ID, func(a *Attempt) error { a.State = "awaiting_review"; return nil })
	b, err := s.Enqueue(p, i, "manual", true)
	if err != nil {
		t.Fatal(err)
	}
	if b.Generation != 2 {
		t.Fatal(b)
	}
	s.Get("attempt", a.ID, &a)
	if a.State != "superseded" || a.Active() || a.Runnable() {
		t.Fatal(a)
	}
	// Simulate an old database written before supersession was implemented.
	a.State = "awaiting_review"
	s.Put("attempt", a.ID, a)
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Get("attempt", a.ID, &a)
	if a.State != "superseded" {
		t.Fatal("historical review not repaired")
	}
}
