package transcript

import (
	"changathi/internal/core"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func event(seq int64, id, mode, text string) core.Event {
	return core.Event{Seq: seq, TurnID: "turn", Created: time.Unix(seq, 0), Output: &core.Output{Kind: "assistant", ItemID: id, Mode: mode, Text: text, Status: "running"}}
}
func TestReplayInterleavedItemsAndFinalSnapshots(t *testing.T) {
	var tr Transcript
	es := []core.Event{event(1, "a", "delta", "Hel"), event(2, "b", "delta", "Other"), event(3, "a", "delta", "lo")}
	final := event(4, "a", "snapshot", "Hello")
	final.Output.Status = "completed"
	es = append(es, final, event(5, "a", "delta", "late"))
	for _, e := range es {
		tr.Apply(e)
	}
	for _, e := range es {
		tr.Apply(e)
	}
	if len(tr.Items) != 2 || tr.Items[0].Text != "Hello" || tr.Items[1].Text != "Other" || !tr.Items[0].Complete || tr.Items[0].Status != "completed" {
		t.Fatal(tr.Items)
	}
	// Reused item IDs in another turn represent a new message.
	next := event(6, "a", "snapshot", "Next turn")
	next.TurnID = "next"
	tr.Apply(next)
	if len(tr.Items) != 3 {
		t.Fatal(tr.Items)
	}
}
func TestLiteralOutputSanitizationAndBounds(t *testing.T) {
	text := "hello\x1b[2J\x1b]52;c;c2VjcmV0\a\x00\x07\n世界\t"
	if got := Clean(text); got != "hello\n世界\t" {
		t.Fatalf("%q", got)
	}
	var tr Transcript
	for n := 1; n <= MaxItems+50; n++ {
		tr.Apply(event(int64(n), fmt.Sprint(n), "snapshot", strings.Repeat("界", 1000)))
	}
	if len(tr.Items) > MaxItems || !tr.Trimmed {
		t.Fatal(len(tr.Items))
	}
	tr.Apply(event(1000, "huge", "delta", strings.Repeat("界", MaxText)))
	last := tr.Items[len(tr.Items)-1]
	if !utf8.ValidString(last.Text) || len(last.Text) > MaxText+100 {
		t.Fatal("unbounded or invalid text")
	}
	r := NewRenderer(32, true, true)
	if strings.Contains(r.Render(last, true, false), "\x1b") {
		t.Fatal("plain output contains ANSI")
	}
}
func TestToolSnapshotAndMessageDelivery(t *testing.T) {
	var tr Transcript
	start := event(1, "tool", "start", "")
	start.Output.Kind = "tool"
	start.Output.Title = "go test ./..."
	tr.Apply(start)
	delta := event(2, "tool", "delta", "FAIL: expected x\n")
	delta.Output.Kind = "tool"
	tr.Apply(delta)
	end := event(3, "tool", "snapshot", "")
	end.Output.Kind = "tool"
	end.Output.Status = "failed"
	tr.Apply(end)
	if tr.Items[0].Text != "FAIL: expected x\n" || tr.Items[0].Status != "failed" {
		t.Fatal(tr.Items)
	}
	m := core.Message{ID: "m", AttemptID: "attempt", Text: "Please retry", State: "queued", Created: time.Unix(2, 0)}
	tr.Messages([]core.Message{m}, "attempt")
	m.State = "acknowledged"
	tr.Messages([]core.Message{m}, "attempt")
	if len(tr.Items) != 2 || tr.Items[1].Status != "acknowledged" {
		t.Fatal(tr.Items)
	}
}

func BenchmarkLargeTranscriptRender(b *testing.B) {
	items := make([]Item, 200)
	for n := range items {
		items[n] = Item{Key: fmt.Sprint(n), Kind: "assistant", Text: "## Result\n\nThe change passed validation.\n\n```go\nfmt.Println(\"hello\")\n```", Revision: 1}
	}
	r := NewRenderer(100, false, true)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for _, item := range items {
			r.Render(item, true, false)
		}
	}
}
