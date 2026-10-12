// Package transcript assembles persisted events into bounded, replayable items.
package transcript

import (
	"changathi/internal/core"
	"changathi/internal/provider"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

const MaxItems = 500
const MaxText = 64 * 1024
const MaxBytes = 2 * 1024 * 1024

type Item struct {
	Key, Kind, Title, Text, Status string
	Created                        time.Time
	DurationMS                     int64
	Complete                       bool
	Revision                       int
}

type Transcript struct {
	Items   []Item
	Last    int64
	Trimmed bool
	index   map[string]int
}

func Clean(s string) string {
	s = ansi.Strip(strings.ToValidUTF8(s, ""))
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

func bounded(s string) string {
	if len(s) <= MaxText {
		return s
	}
	return strings.ToValidUTF8(s[:MaxText], "") + "\n[output truncated]"
}

func (t *Transcript) rebuild() {
	t.index = make(map[string]int, len(t.Items))
	for n, i := range t.Items {
		t.index[i.Key] = n
	}
}

func (t *Transcript) put(i Item) {
	if t.index == nil {
		t.rebuild()
	}
	if n, ok := t.index[i.Key]; ok {
		old := &t.Items[n]
		i.Created = old.Created
		i.Revision = old.Revision + 1
		*old = i
	} else {
		i.Revision = 1
		t.index[i.Key] = len(t.Items)
		t.Items = append(t.Items, i)
	}
	bytes := 0
	for _, item := range t.Items {
		bytes += len(item.Text) + len(item.Title)
	}
	for len(t.Items) > MaxItems || bytes > MaxBytes {
		bytes -= len(t.Items[0].Text) + len(t.Items[0].Title)
		t.Items = t.Items[1:]
		t.Trimmed = true
		t.index = nil
	}
}

// Apply deduplicates ordered event pages. Replaying older pages should use a new
// Transcript with all resident events sorted by sequence.
func (t *Transcript) Apply(e core.Event) {
	if e.Seq > 0 && e.Seq <= t.Last {
		return
	}
	if e.Seq > t.Last {
		t.Last = e.Seq
	}
	o := e.Output
	if o == nil && strings.Contains(e.Type, "/") {
		// Records without a stored payload (legacy, or written before a method was
		// normalized) keep the raw params as text. Legacy deltas hold bare text with
		// no item identity; their completed snapshot carries the full message.
		o = provider.NormalizeOutput(e.Type, json.RawMessage(e.Text), e.TurnID)
		if o == nil {
			// Protocol bookkeeping remains available in the raw view.
			return
		}
	}
	if o == nil {
		switch e.Type {
		case "session_started":
			return
		case "state":
			state, reason, _ := strings.Cut(e.Text, ":")
			t.put(Item{Key: fmt.Sprintf("event:%d", e.Seq), Kind: "lifecycle", Title: StateLabel(Clean(state)), Text: bounded(Clean(strings.TrimSpace(reason))), Created: e.Created, Complete: true})
			return
		}
		t.put(Item{Key: fmt.Sprintf("event:%d", e.Seq), Kind: "event", Title: Clean(e.Type), Text: bounded(Clean(e.Text)), Created: e.Created, Complete: true})
		return
	}
	key := e.TurnID + ":" + o.ItemID
	if o.ItemID == "" {
		key = fmt.Sprintf("event:%d", e.Seq)
	}
	if t.index == nil {
		t.rebuild()
	}
	i := Item{Key: key, Kind: o.Kind, Created: e.Created}
	if n, ok := t.index[key]; ok {
		i = t.Items[n]
	}
	if i.Complete && (o.Mode == "delta" || o.Mode == "start") {
		return
	}
	if o.Title != "" {
		i.Title = bounded(Clean(o.Title))
	}
	if o.Kind != "" {
		i.Kind = o.Kind
	}
	if o.Status != "" {
		i.Status = o.Status
	}
	if o.DurationMS > 0 {
		i.DurationMS = o.DurationMS
	}
	switch o.Mode {
	case "delta":
		if !i.Complete && len(i.Text) <= MaxText {
			i.Text = bounded(i.Text + Clean(o.Text))
		}
	case "snapshot":
		// Empty terminal snapshots occur on some tools; preserve streamed output.
		if o.Text != "" || i.Kind == "assistant" {
			i.Text = bounded(Clean(o.Text))
		}
		i.Complete = i.Status != "running"
	case "start":
		if i.Text == "" {
			i.Text = bounded(Clean(o.Text))
		}
	}
	t.put(i)
}
