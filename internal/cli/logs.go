package cli

import (
	"changathi/internal/core"
	"changathi/internal/transcript"
	"context"
	"fmt"
	"os"
	"sort"
	"time"
)

type logOptions struct {
	follow, raw, full, last bool
	tail                    int
}

func (a *app) readableLogs(ctx context.Context, id string, o logOptions) error {
	follow, raw := o.follow, o.raw
	client := a.client()
	page, err := client.Transcript(ctx, id, 0, 0)
	if err != nil {
		return err
	}
	events := append([]core.Event(nil), page.Events...)
	// Load retained history in bounded pages. The viewer/log buffer remains bounded.
	before, more := page.Before, page.HasMore
	for more && len(events) < 2000 {
		older, e := client.Transcript(ctx, id, 0, before)
		if e != nil {
			return e
		}
		events = append(events, older.Events...)
		before, more = older.Before, older.HasMore
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	if raw && o.tail > 0 && len(events) > o.tail {
		events = events[len(events)-o.tail:]
	}
	renderer := transcript.Renderer{Full: o.full}
	t := transcript.Transcript{}
	printed := map[string]bool{}
	write := func(es []core.Event, initial bool) error {
		if raw {
			for _, e := range es {
				if a.json {
					if err := a.print(e); err != nil {
						return err
					}
				} else {
					if _, err := fmt.Fprintf(a.out, "%s %s %s\n", e.Created.Format(time.RFC3339), transcript.Clean(e.Type), transcript.Clean(e.Text)); err != nil {
						return err
					}
				}
			}
			return nil
		}
		for _, e := range es {
			t.Apply(e)
		}
		if initial {
			if s, e := client.Status(ctx); e == nil {
				t.Messages(s.Messages, id)
			}
			// --last and --tail skip everything before the requested window.
			skip := 0
			if o.tail > 0 {
				skip = len(t.Items) - o.tail
			}
			if o.last {
				skip = len(t.Items)
				for n := len(t.Items) - 1; n >= 0; n-- {
					if t.Items[n].Kind == "assistant" {
						skip = n
						break
					}
				}
			}
			for n := 0; n < skip && n < len(t.Items); n++ {
				printed[t.Items[n].Key] = true
			}
		}
		for _, i := range t.Items {
			if printed[i.Key] || (!i.Complete && follow) {
				continue
			}
			if _, err := fmt.Fprint(a.out, renderer.Render(i)); err != nil {
				return err
			}
			printed[i.Key] = true
		}
		if len(printed) > transcript.MaxItems*2 {
			clear(printed)
			for _, i := range t.Items {
				if i.Complete {
					printed[i.Key] = true
				}
			}
		}
		return nil
	}
	if (page.Gap || more) && o.tail == 0 && !o.last {
		if a.json {
			fmt.Fprintln(os.Stderr, "[Earlier history is incomplete or outside the viewer limit]")
		} else {
			fmt.Fprintln(a.out, "[Earlier history is incomplete or outside the viewer limit]")
		}
	}
	if err = write(events, true); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	after := page.Cursor
	feedID := page.FeedID
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
		p, e := client.Transcript(ctx, id, after, 0)
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		if p.Gap && !a.json {
			fmt.Fprintln(a.out, "[History gap: resuming from retained events]")
		}
		if p.FeedID != feedID {
			feedID = p.FeedID
			after = 0
			t = transcript.Transcript{}
			clear(printed)
			if !a.json {
				fmt.Fprintln(a.out, "[Daemon history replaced; reloading]")
			}
			continue
		}
		if p.Gap {
			t = transcript.Transcript{}
		}
		after = p.Cursor
		if err = write(p.Events, false); err != nil {
			return err
		}
	}
}
