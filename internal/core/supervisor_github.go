package core

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Closure struct {
	ID    int64  `json:"id"`
	Event string `json:"event"`
	Actor struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"actor"`
	CommitID  string    `json:"commit_id"`
	CreatedAt time.Time `json:"created_at"`
	App       any       `json:"performed_via_github_app"`
}
type DeliveryEvidence struct {
	Number   int       `json:"number"`
	URL      string    `json:"html_url"`
	MergedAt time.Time `json:"merged_at"`
	Head     struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"base"`
}

// Optional interface keeps existing unsupervised adapters usable.
type SupervisorGitHub interface {
	Operator(context.Context) (string, error)
	Closure(context.Context, Project, int) (Closure, error)
	Delivery(context.Context, Project, int) (DeliveryEvidence, error)
	SupervisorWorkpad(context.Context, Project, int, string, string) (int64, error)
}

func (g GH) Operator(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if err := g.api(ctx, "user", &u); err != nil {
		return "", err
	}
	if u.Login == "" || u.Type != "User" {
		return "", fmt.Errorf("Changathi requires a human GitHub operator login")
	}
	return u.Login, nil
}
func (g GH) Closure(ctx context.Context, p Project, n int) (Closure, error) {
	var last Closure
	for page := 1; ; page++ {
		var events []Closure
		if err := g.api(ctx, fmt.Sprintf("repos/%s/issues/%d/events?per_page=100&page=%d", p.Repo, n, page), &events); err != nil {
			return last, err
		}
		for _, ev := range events {
			if ev.Event == "closed" || ev.Event == "reopened" {
				last = ev
			}
		}
		if len(events) < 100 {
			return last, nil
		}
	}
}
func (g GH) Delivery(ctx context.Context, p Project, n int) (DeliveryEvidence, error) {
	var v DeliveryEvidence
	err := g.api(ctx, fmt.Sprintf("repos/%s/pulls/%d", p.Repo, n), &v)
	return v, err
}
func (g GH) SupervisorWorkpad(ctx context.Context, p Project, n int, id, body string) (int64, error) {
	return g.upsertComment(ctx, p, n, "<!-- changathi-supervisor:"+id+" -->", body)
}

// upsertComment edits the operator's comment on issue n that carries marker, or
// creates one. The stable marker recovers a successful POST whose response was lost.
func (g GH) upsertComment(ctx context.Context, p Project, n int, marker, body string) (int64, error) {
	user, err := g.Operator(ctx)
	if err != nil {
		return 0, err
	}
	var commentID int64
	for page := 1; ; page++ {
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := g.api(ctx, fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100&page=%d", p.Repo, n, page), &comments); err != nil {
			return 0, err
		}
		for _, c := range comments {
			if c.User.Login == user && strings.Contains(c.Body, marker) {
				commentID = c.ID
				break
			}
		}
		if commentID != 0 || len(comments) < 100 {
			break
		}
	}
	path := fmt.Sprintf("repos/%s/issues/%d/comments", p.Repo, n)
	method := "POST"
	if commentID != 0 {
		path = fmt.Sprintf("repos/%s/issues/comments/%d", p.Repo, commentID)
		method = "PATCH"
	}
	var response struct {
		ID int64 `json:"id"`
	}
	err = g.api(ctx, path, &response, "--method", method, "-f", "body="+marker+"\n"+body)
	if err != nil {
		return 0, err
	}
	if response.ID == 0 {
		return 0, fmt.Errorf("comment response missing comment ID")
	}
	return response.ID, nil
}

// EvidenceComments is optional so older adapters remain compatible.
type EvidenceComments interface {
	EvidenceComments(context.Context, Project, int) ([]EvidenceComment, error)
}
type EvidenceComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	URL       string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func (g GH) EvidenceComments(ctx context.Context, p Project, n int) ([]EvidenceComment, error) {
	var all []EvidenceComment
	for page := 1; ; page++ {
		var batch []EvidenceComment
		if err := g.api(ctx, fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100&page=%d", p.Repo, n, page), &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

type EvidenceReportFinder interface {
	EvidenceReport(context.Context, Project, int) (string, error)
}

func (g GH) EvidenceReport(ctx context.Context, p Project, pr int) (string, error) {
	delivery, err := g.Delivery(ctx, p, pr)
	if err != nil {
		return "", err
	}
	var reports []string
	for page := 1; ; page++ {
		var files []struct {
			Name   string `json:"filename"`
			Status string `json:"status"`
		}
		if err := g.api(ctx, fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100&page=%d", p.Repo, pr, page), &files); err != nil {
			return "", err
		}
		for _, f := range files {
			if f.Status != "removed" && strings.HasPrefix(f.Name, "docs/validation/") && strings.HasSuffix(f.Name, ".md") {
				reports = append(reports, f.Name)
			}
		}
		if len(files) < 100 {
			break
		}
	}
	if len(reports) == 1 && delivery.Head.SHA != "" {
		return fmt.Sprintf("https://github.com/%s/blob/%s/%s", p.Repo, delivery.Head.SHA, reports[0]), nil
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", p.Repo, pr), nil
}
