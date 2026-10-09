package service

import (
	"changathi/internal/core"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestTranscriptAPI(t *testing.T) {
	e, _, _ := setup(t, 1, &fakeProvider{})
	e.Store.AppendEvent(core.Event{Version: 1, AttemptID: "one", TurnID: "turn", Output: &core.Output{Kind: "assistant", ItemID: "message", Mode: "snapshot", Text: "hello", Status: "completed"}})
	e.Store.Event("two", "message", "unrelated")
	s := &Server{Engine: e}
	r := httptest.NewRequest("GET", "/v1/transcript?attempt=one&limit=1", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var response struct {
		Data  core.EventPage `json:"data"`
		Error string         `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" || len(response.Data.Events) != 1 || response.Data.Events[0].Output.Text != "hello" || response.Data.FeedID == "" {
		t.Fatal(w.Body.String())
	}
	for _, query := range []string{"attempt=one&limit=999", "attempt=one&after=invalid", "attempt=one&after=1&before=2"} {
		w = httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "/v1/transcript?"+query, nil))
		if w.Code != 400 {
			t.Fatal("invalid query accepted", query)
		}
	}
}
