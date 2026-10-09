package core

import (
	"strings"
	"sync"
	"testing"
)

func TestSupervisorParser(t *testing.T) {
	good := "prose\n```changathi-supervisor\nversion: 1\nchildren:\n - issue: 3\n - issue: 4\n   depends_on: [3]\n```\n"
	p, h, err := ParseSupervisor(good, 10)
	if err != nil || len(p.Children) != 2 || h == "" {
		t.Fatal(p, h, err)
	}
	_, h2, err := ParseSupervisor(strings.Replace(good, "prose", "different prose", 1), 10)
	if err != nil || h != h2 {
		t.Fatal("narrative changed graph hash")
	}
	bad := []string{
		strings.Replace(good, "version: 1", "version: 2", 1),
		strings.Replace(good, "version: 1", "version: 1\nunknown: true", 1),
		strings.Replace(good, "issue: 4", "issue: 3", 1),
		strings.Replace(good, "issue: 4", "issue: 10", 1),
		strings.Replace(good, "[3]", "[9]", 1),
		strings.Replace(good, "[3]", "[3,3]", 1),
		strings.Replace(good, " - issue: 3", " - issue: 3\n   depends_on: [4]", 1),
		strings.Replace(good, " - issue: 3", " - issue: 3\n   completion: whatever", 1),
		good + good, strings.TrimSuffix(good, "```\n"),
		strings.Replace(good, "version: 1", "version: 1\n---\nversion: 1", 1),
	}
	for _, body := range bad {
		if _, _, err := ParseSupervisor(body, 10); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}
func TestSupervisorAtomicOwnership(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := Project{ID: ID(), RepoID: 1, Name: "test"}
	i := Issue{ID: 2, Number: 2, State: "open"}
	v := Supervisor{ID: ID(), ProjectID: p.ID, Parent: Issue{ID: 1, Number: 1}, Revision: "rev", Plan: SupervisorPlan{Version: 1, Children: []SupervisorChild{{Issue: 2}}}, Evidence: map[int]ChildEvidence{2: {IssueID: 2}}, Current: 2}
	if err = s.EnrollSupervisor(v); err != nil {
		t.Fatal(err)
	}
	other := v
	other.ID = ID()
	other.Parent = Issue{ID: 3, Number: 3}
	if err = s.EnrollSupervisor(other); err == nil {
		t.Fatal("duplicate ownership accepted")
	}
	if _, err = s.Enqueue(p, i, "manual", false); err == nil {
		t.Fatal("ordinary enqueue bypassed supervisor")
	}
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.EnqueueSupervised(p, i, v, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	as, _ := List[Attempt](s, "attempt")
	if len(as) != 1 || as[0].SupervisorID != v.ID {
		t.Fatal(as)
	}
	plans, _ := List[Supervisor](s, "supervisor")
	if len(plans) != 1 {
		t.Fatal("failed enrollment partially persisted")
	}
}
