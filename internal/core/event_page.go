package core

import (
	"encoding/json"
	"fmt"
	"slices"
)

type EventPage struct {
	FeedID  string  `json:"feed_id"`
	Events  []Event `json:"events"`
	Cursor  int64   `json:"cursor"`
	Before  int64   `json:"before"`
	HasMore bool    `json:"has_more"`
	Gap     bool    `json:"gap"`
}

// EventPage reads a consistent snapshot. Initial requests return the most recent
// page; after follows forward, before pages backward. Cursor includes unrelated
// events when caught up, so idle agents do not repeatedly scan the same history.
func (s *Store) EventPage(attempt string, after, before int64, limit int) (EventPage, error) {
	p := EventPage{FeedID: s.FeedID, Events: []Event{}}
	if limit < 1 || limit > 500 || after < 0 || before < 0 || (after > 0 && before > 0) || attempt == "" {
		return p, fmt.Errorf("attempt and valid cursor/limit required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	var first, latest int64
	if err = tx.QueryRow(`SELECT coalesce(min(seq),0),coalesce(max(seq),0) FROM events`).Scan(&first, &latest); err != nil {
		return p, err
	}
	p.Gap = first > 1 && (after == 0 || after < first-1)
	if after > 0 && first > after+1 {
		after = 0
	}
	// A replaced database cannot honor a cursor from its previous generation.
	if after > latest {
		p.Gap = true
		after = 0
	}
	query := `SELECT seq,data FROM events WHERE json_extract(data,'$.attempt_id')=?`
	args := []any{attempt}
	if after > 0 {
		query += ` AND seq>?`
		args = append(args, after)
	}
	if before > 0 {
		query += ` AND seq<?`
		args = append(args, before)
	}
	if after > 0 {
		query += ` ORDER BY seq ASC`
	} else {
		query += ` ORDER BY seq DESC`
	}
	query += ` LIMIT ?`
	args = append(args, limit+1)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var e Event
		var b []byte
		if err = rows.Scan(&e.Seq, &b); err != nil {
			rows.Close()
			return p, err
		}
		seq := e.Seq
		if err = json.Unmarshal(b, &e); err != nil {
			rows.Close()
			return p, err
		}
		e.Seq = seq
		p.Events = append(p.Events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	p.HasMore = len(p.Events) > limit
	if p.HasMore {
		p.Events = p.Events[:limit]
	}
	if after == 0 {
		slices.Reverse(p.Events)
	}
	p.Cursor = latest
	if len(p.Events) > 0 {
		p.Before = p.Events[0].Seq
		if after > 0 && p.HasMore {
			p.Cursor = p.Events[len(p.Events)-1].Seq
		}
	}
	return p, tx.Commit()
}
