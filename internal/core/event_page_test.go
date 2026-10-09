package core

import "testing"

func TestTranscriptPagesAndLiveBoundary(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for n := 0; n < 12; n++ {
		id := "a"
		if n%2 != 0 {
			id = "b"
		}
		if err = s.Event(id, "message", "text"); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.EventPage("a", 0, 0, 2)
	if err != nil || len(p.Events) != 2 || p.Events[0].Seq != 9 || p.Cursor != 12 || !p.HasMore {
		t.Fatalf("%+v %v", p, err)
	}
	older, err := s.EventPage("a", 0, p.Before, 2)
	if err != nil || len(older.Events) != 2 || older.Events[0].Seq != 5 {
		t.Fatalf("%+v %v", older, err)
	}
	s.Event("b", "message", "unrelated")
	s.Event("a", "message", "new")
	live, err := s.EventPage("a", p.Cursor, 0, 2)
	if err != nil || len(live.Events) != 1 || live.Events[0].Text != "new" || live.Cursor != 14 {
		t.Fatalf("%+v %v", live, err)
	}
	idle, _ := s.EventPage("a", live.Cursor, 0, 2)
	if len(idle.Events) != 0 || idle.Cursor != 14 {
		t.Fatal(idle)
	}
}
func TestTranscriptGapAndValidation(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.EventLimit = 100
	for n := 0; n < 200; n++ {
		s.Event("a", "message", "text")
	}
	p, err := s.EventPage("a", 1, 0, 20)
	if err != nil || !p.Gap {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err = s.EventPage("a", 1, 2, 20); err == nil {
		t.Fatal("mixed cursors accepted")
	}
	if _, err = s.EventPage("a", 0, 0, 501); err == nil {
		t.Fatal("unbounded limit")
	}
}

func TestEventFeedSurvivesRestartAndIdentifiesReplacement(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := s.FeedID
	if err = s.AppendEvent(Event{Version: 1, AttemptID: "a", Output: &Output{Kind: "assistant", ItemID: "m", Text: "durable", Mode: "snapshot"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.EventPage("a", 0, 0, 200)
	if err != nil || p.FeedID != id || len(p.Events) != 1 || p.Events[0].Output.Text != "durable" {
		t.Fatalf("%+v %v", p, err)
	}
	other, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.FeedID == id {
		t.Fatal("replacement indistinguishable")
	}
}
