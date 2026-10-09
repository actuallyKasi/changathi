package cli

import (
	"bytes"
	"changathi/internal/core"
	"changathi/internal/service"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type Client struct{ HTTP *http.Client }

type ResponseError struct{ Message string }

func (e *ResponseError) Error() string { return e.Message }

func NewClient(socket string) *Client {
	return &Client{HTTP: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}, Timeout: 2 * time.Minute}}
}
func (c *Client) call(ctx context.Context, method, path string, input, out any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, "http://changathi"+path, body)
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(r)
	if err != nil {
		return fmt.Errorf("cannot reach daemon: %w (start with changathi daemon start)", err)
	}
	defer res.Body.Close()
	var v struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if err = json.NewDecoder(res.Body).Decode(&v); err != nil {
		return err
	}
	if v.Error != "" {
		return &ResponseError{Message: v.Error}
	}
	if out != nil && len(v.Data) > 0 {
		return json.Unmarshal(v.Data, out)
	}
	return nil
}
func (c *Client) Action(ctx context.Context, r service.Request, out any) error {
	if r.ID == "" {
		r.ID = core.ID()
	}
	return c.call(ctx, "POST", "/v1/action", r, out)
}
func (c *Client) Status(ctx context.Context) (core.Snapshot, error) {
	var s core.Snapshot
	err := c.call(ctx, "GET", "/v1/status", nil, &s)
	return s, err
}
func (c *Client) Events(ctx context.Context, after int64) ([]core.Event, error) {
	var es []core.Event
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/events?after=%d", after), nil, &es)
	return es, err
}

func (c *Client) Transcript(ctx context.Context, attempt string, after, before int64) (core.EventPage, error) {
	var page core.EventPage
	q := url.Values{"attempt": {attempt}, "after": {fmt.Sprint(after)}, "before": {fmt.Sprint(before)}}
	err := c.call(ctx, "GET", "/v1/transcript?"+q.Encode(), nil, &page)
	return page, err
}
