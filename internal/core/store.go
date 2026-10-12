package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Store struct {
	db         *sql.DB
	mu         sync.Mutex
	EventLimit int
	Dir        string
	FeedID     string
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "state.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS records(kind TEXT NOT NULL,id TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(kind,id));
 CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY AUTOINCREMENT,data BLOB NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, EventLimit: 10000, Dir: dir}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS events_attempt ON events(json_extract(data,'$.attempt_id'),seq)`); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.supersedeReviews(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.Get("meta", "event_feed", &s.FeedID); err != nil {
		if err != sql.ErrNoRows {
			db.Close()
			return nil, err
		}
		s.FeedID = ID()
		if err = s.Put("meta", "event_feed", s.FeedID); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) put(kind, id string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("INSERT INTO records VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data", kind, id, b)
	return e
}
func (s *Store) Put(kind, id string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.put(kind, id, v)
}
func (s *Store) Get(kind, id string, v any) error {
	var b []byte
	e := s.db.QueryRow("SELECT data FROM records WHERE kind=? AND id=?", kind, id).Scan(&b)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func List[T any](s *Store, kind string) ([]T, error) {
	rows, e := s.db.Query("SELECT data FROM records WHERE kind=? ORDER BY rowid", kind)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var b []byte
		var v T
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) UpdateAttempt(id string, fn func(*Attempt) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var a Attempt
	if e := s.Get("attempt", id, &a); e != nil {
		return e
	}
	if e := fn(&a); e != nil {
		return e
	}
	a.Updated = Now()
	return s.put("attempt", id, a)
}
func (s *Store) Project(name string) (Project, error) {
	ps, e := List[Project](s, "project")
	if e != nil {
		return Project{}, e
	}
	for _, p := range ps {
		if p.Name == name || p.ID == name {
			return p, nil
		}
	}
	return Project{}, fmt.Errorf("unknown project %q", name)
}

func (s *Store) UpdateProject(id string, fn func(*Project) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p Project
	if err := s.Get("project", id, &p); err != nil {
		return err
	}
	if err := fn(&p); err != nil {
		return err
	}
	return s.put("project", id, p)
}
func (s *Store) AddProject(p Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, e := List[Project](s, "project")
	if e != nil {
		return e
	}
	for _, old := range ps {
		if old.Name == p.Name || old.RepoID == p.RepoID {
			return fmt.Errorf("project name or repository already registered")
		}
	}
	return s.put("project", p.ID, p)
}
func (s *Store) Enqueue(p Project, i Issue, trigger string, retry bool) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enqueueLocked(p, i, trigger, retry, "", "")
}
func (s *Store) enqueueLocked(p Project, i Issue, trigger string, retry bool, supervisor, revision string) (Attempt, error) {
	if i.Supervisor() {
		return Attempt{}, fmt.Errorf("supervisor parents require supervisor start")
	}
	owner, err := s.SupervisorOwner(p.ID, i.ID)
	if err != nil {
		return Attempt{}, err
	}
	if owner != supervisor {
		return Attempt{}, fmt.Errorf("child owned by supervisor %s; use the supervisor dependency gate", owner)
	}
	as, e := List[Attempt](s, "attempt")
	if e != nil {
		return Attempt{}, e
	}
	g := 0
	var latest *Attempt
	for _, a := range as {
		if a.ProjectID != p.ID || a.Issue.ID != i.ID {
			continue
		}
		if a.Generation > g {
			g = a.Generation
			copy := a
			latest = &copy
		}
		if a.Active() {
			return a, nil
		}
	}
	if latest != nil {
		if !retry {
			return *latest, nil
		}
		if latest.State != "awaiting_review" && latest.State != "done" && !strings.Contains(latest.Error, "closed without merging") {
			guard, err := ExecutionLock(s.Dir, latest.ID)
			if err != nil {
				return Attempt{}, err
			}
			guard.Close()
			latest.State = "queued"
			latest.Issue = i
			latest.Trigger = trigger
			latest.SupervisorID = supervisor
			latest.PlanRevision = revision
			latest.Retries = 0
			latest.Turns = 0
			latest.VerifyFailures = 0
			latest.NextStep = ""
			// Re-read WORKFLOW.md from the base branch so an operator retry picks
			// up policy changes such as a new provider; ThreadProvider keeps an
			// old provider's thread from being resumed by the new one.
			latest.Workflow = Workflow{}
			latest.PID = 0
			latest.ReadyRemoved = false
			latest.Due = time.Time{}
			latest.Updated = Now()
			return *latest, s.put("attempt", latest.ID, latest)
		}
	}
	a := Attempt{SupervisorID: supervisor, PlanRevision: revision, ID: ID(), ProjectID: p.ID, Issue: i, Generation: g + 1, Trigger: trigger, State: "queued", Created: Now(), Updated: Now()}
	if latest != nil {
		failure := latest.LastFailure
		if failure == "" && strings.Contains(latest.Error, "closed without merging") {
			failure = latest.Error
		}
		a.Previous = &PreviousGeneration{Generation: latest.Generation, PR: latest.PR, State: latest.State, LastFailure: failure}
	}
	a.Branch = fmt.Sprintf("changathi/%s/%d/g%d", p.ID[:8], i.Number, a.Generation)
	if err := s.put("attempt", a.ID, a); err != nil {
		return a, err
	}
	return a, s.supersedeReviews()
}
func (s *Store) Event(id, typ, text string) error {
	return s.AppendEvent(Event{AttemptID: id, Type: typ, Text: text, Created: Now()})
}
func (s *Store) AppendEvent(event Event) error {
	if event.Created.IsZero() {
		event.Created = Now()
	}
	b, e := json.Marshal(event)
	if e != nil {
		return e
	}
	result, e := s.db.Exec("INSERT INTO events(data) VALUES(?)", b)
	if e == nil {
		seq, _ := result.LastInsertId()
		if seq%100 == 0 {
			_, e = s.db.Exec("DELETE FROM events WHERE seq<=?", seq-int64(s.EventLimit))
		}
	}
	return e
}
func (s *Store) Events(after int64, limit int) ([]Event, error) {
	if after > 0 {
		var first sql.NullInt64
		if e := s.db.QueryRow("SELECT min(seq) FROM events").Scan(&first); e != nil {
			return nil, e
		}
		if first.Valid && first.Int64 > after+1 {
			return nil, fmt.Errorf("event cursor expired; refresh status and restart log streaming")
		}
	}
	rows, e := s.db.Query("SELECT seq,data FROM events WHERE seq>? ORDER BY seq LIMIT ?", after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var b []byte
		var seq int64
		var v Event
		if e = rows.Scan(&seq, &b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		v.Seq = seq
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Snapshot() (Snapshot, error) {
	var v Snapshot
	var e error
	v.Supervisors, e = List[Supervisor](s, "supervisor")
	if e != nil {
		return v, e
	}
	v.Projects, e = List[Project](s, "project")
	if e != nil {
		return v, e
	}
	v.Agents, e = List[Attempt](s, "attempt")
	if e != nil {
		return v, e
	}
	for _, name := range Providers {
		var usage any
		if s.Get("usage", name, &usage) == nil && usage != nil {
			if v.Usage == nil {
				v.Usage = map[string]any{}
			}
			v.Usage[name] = usage
		}
	}
	gates, _ := List[ProviderGate](s, "gate")
	for _, gate := range gates {
		if gate.Until.After(Now()) {
			if gate.Provider == "" {
				gate.Provider = "codex"
			}
			v.ProviderWaits = append(v.ProviderWaits, gate)
			if v.ProviderWait == nil || gate.Until.Before(v.ProviderWait.Until) {
				g := gate
				v.ProviderWait = &g
			}
		}
	}
	return v, e
}

// Repairs historical review rows as well as new retries. A completed delivery
// remains done; only an obsolete pending review becomes superseded.
func (s *Store) supersedeReviews() error {
	as, err := List[Attempt](s, "attempt")
	if err != nil {
		return err
	}
	latest := map[string]Attempt{}
	key := func(a Attempt) string { return fmt.Sprintf("%s/%d", a.ProjectID, a.Issue.ID) }
	for _, a := range as {
		if a.Generation > latest[key(a)].Generation {
			latest[key(a)] = a
		}
	}
	for _, a := range as {
		next := latest[key(a)]
		if a.State == "awaiting_review" && a.Generation < next.Generation {
			a.State = "superseded"
			a.Activity = "replaced by " + next.ID
			a.Updated = Now()
			if err := s.put("attempt", a.ID, a); err != nil {
				return err
			}
		}
	}
	return nil
}
