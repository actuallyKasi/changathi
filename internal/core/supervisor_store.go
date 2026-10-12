package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

func ownerKey(project string, issue int64) string { return fmt.Sprintf("%s/%d", project, issue) }
func (s *Store) SupervisorOwner(project string, issue int64) (string, error) {
	var owner string
	err := s.Get("supervisor_owner", ownerKey(project, issue), &owner)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return owner, err
}
func (s *Store) SupervisorFor(project string, number int) (Supervisor, error) {
	plans, err := List[Supervisor](s, "supervisor")
	if err != nil {
		return Supervisor{}, err
	}
	for _, v := range plans {
		if v.ProjectID == project && v.Parent.Number == number {
			return v, nil
		}
	}
	return Supervisor{}, fmt.Errorf("supervisor #%d is not enrolled", number)
}

// EnrollSupervisor writes plan/revision/ownership in one transaction, serialized with enqueue.
func (s *Store) EnrollSupervisor(plan Supervisor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkSupervisorLocked(plan); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	put := func(kind, id string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO records(kind,id,data) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data", kind, id, b)
		return err
	}
	var old Supervisor
	// Existing ownership is kept for retained children and released only by a validated reload.
	var b []byte
	if err = tx.QueryRow("SELECT data FROM records WHERE kind='supervisor' AND id=?", plan.ID).Scan(&b); err == nil {
		if err = json.Unmarshal(b, &old); err != nil {
			return err
		}
		for n, ev := range old.Evidence {
			if _, kept := plan.Evidence[n]; !kept {
				if _, err = tx.Exec("DELETE FROM records WHERE kind='supervisor_owner' AND id=?", ownerKey(plan.ProjectID, ev.IssueID)); err != nil {
					return err
				}
			}
		}
	} else if err != sql.ErrNoRows {
		return err
	}
	for _, ev := range plan.Evidence {
		if err = put("supervisor_owner", ownerKey(plan.ProjectID, ev.IssueID), plan.ID); err != nil {
			return err
		}
	}
	if err = put("supervisor_revision", plan.ID+"/"+plan.Revision, plan.Plan); err != nil {
		return err
	}
	if err = put("supervisor", plan.ID, plan); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ValidateSupervisor(plan Supervisor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkSupervisorLocked(plan)
}
func (s *Store) checkSupervisorLocked(plan Supervisor) error {
	plans, err := List[Supervisor](s, "supervisor")
	if err != nil {
		return err
	}
	for _, other := range plans {
		if other.ID != plan.ID && other.ProjectID == plan.ProjectID && other.Parent.ID == plan.Parent.ID {
			return fmt.Errorf("parent already enrolled")
		}
	}
	if owner, err := s.SupervisorOwner(plan.ProjectID, plan.Parent.ID); err != nil || owner != "" {
		return fmt.Errorf("parent is already an owned child: %s (%v)", owner, err)
	}
	as, err := List[Attempt](s, "attempt")
	if err != nil {
		return err
	}
	for _, a := range as {
		if a.ProjectID != plan.ProjectID {
			continue
		}
		_, child := plan.Child(a.Issue.Number)
		if a.Issue.ID == plan.Parent.ID && (a.Active() || a.State == "awaiting_review") {
			return fmt.Errorf("parent has an unfinished implementation attempt")
		}
		if child && a.SupervisorID != plan.ID && (a.Active() || a.State == "awaiting_review") {
			return fmt.Errorf("child #%d already has an unfinished attempt", a.Issue.Number)
		}
	}
	for _, ev := range plan.Evidence {
		owner, err := s.SupervisorOwner(plan.ProjectID, ev.IssueID)
		if err != nil {
			return err
		}
		if owner != "" && owner != plan.ID {
			return fmt.Errorf("child already belongs to supervisor %s", owner)
		}
		for _, other := range plans {
			if other.ProjectID == plan.ProjectID && other.Parent.ID == ev.IssueID {
				return fmt.Errorf("nested supervisor is unsupported")
			}
		}
	}
	return nil
}
func (s *Store) EnqueueSupervised(p Project, i Issue, plan Supervisor, retry bool) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current Supervisor
	if err := s.Get("supervisor", plan.ID, &current); err != nil {
		return Attempt{}, err
	}
	if current.Revision != plan.Revision || current.Hold != "" || !current.Selected(i.Number) {
		return Attempt{}, fmt.Errorf("supervisor changed or child is not selected")
	}
	as, err := List[Attempt](s, "attempt")
	if err != nil {
		return Attempt{}, err
	}
	latest := map[int64]Attempt{}
	for _, a := range as {
		if a.ProjectID == p.ID && a.Generation > latest[a.Issue.ID].Generation {
			latest[a.Issue.ID] = a
		}
	}
	// Explicit enrollment may resume an inactive previous attempt. Active and
	// awaiting-review external attempts were rejected during enrollment.
	if old, ok := latest[i.ID]; ok && old.SupervisorID != plan.ID {
		retry = true
	}
	return s.enqueueLocked(p, i, "supervisor", retry, plan.ID, plan.Revision)
}
