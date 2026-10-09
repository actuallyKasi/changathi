package core

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type GitHub interface {
	Repository(context.Context, string) (Project, error)
	Issue(context.Context, Project, int) (Issue, error)
	Issues(context.Context, Project, bool) ([]Issue, error)
	FindPR(context.Context, Project, string) (*PR, error)
	CreatePR(context.Context, Project, Attempt) (*PR, error)
	Merge(context.Context, Project, *PR, string) error
	MarkReview(context.Context, Project, int) error
	MutateIssue(context.Context, Project, int, string, map[string]any) (any, error)
}
type GH struct{}

func (GH) api(ctx context.Context, path string, out any, args ...string) error {
	all := append([]string{"api", path}, args...)
	b, e := Command(ctx, "", "gh", all...)
	if e != nil {
		return e
	}
	if out != nil {
		return json.Unmarshal([]byte(b), out)
	}
	return nil
}
func (g GH) Repository(ctx context.Context, repo string) (Project, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !regexp.MustCompile(`^[A-Za-z0-9-]+$`).MatchString(parts[0]) || !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
		return Project{}, fmt.Errorf("repository must be owner/name on github.com")
	}
	var v struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		CloneURL      string `json:"clone_url"`
	}
	e := g.api(ctx, "repos/"+repo, &v)
	return Project{ID: ID(), Repo: v.FullName, RepoID: v.ID, Base: v.DefaultBranch, CloneURL: v.CloneURL, Enabled: true, Concurrency: 1}, e
}
func (g GH) Issue(ctx context.Context, p Project, n int) (Issue, error) {
	var i Issue
	e := g.api(ctx, fmt.Sprintf("repos/%s/issues/%d", p.Repo, n), &i)
	if e == nil && i.PullRequest != nil {
		e = fmt.Errorf("#%d is a pull request, not an issue", n)
	}
	return i, e
}
func (g GH) Issues(ctx context.Context, p Project, ready bool) ([]Issue, error) {
	path := "repos/" + p.Repo + "/issues?state=open&per_page=100&sort=created&direction=asc"
	if ready {
		path += "&labels=agent%3Aready"
	}
	out := []Issue{}
	for page := 1; ; page++ {
		var xs []Issue
		if e := g.api(ctx, path+"&page="+strconv.Itoa(page), &xs); e != nil {
			return nil, e
		}
		for _, i := range xs {
			if i.PullRequest == nil {
				out = append(out, i)
			}
		}
		if len(xs) < 100 {
			return out, nil
		}
	}
}

type PR struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	State      string `json:"state"`
	Head       string `json:"headRefOid"`
	Branch     string `json:"headRefName"`
	Base       string `json:"baseRefName"`
	BaseSHA    string `json:"baseRefOid"`
	MergeState string `json:"mergeStateStatus"`
	Mergeable  string `json:"mergeable"`
	Body       string `json:"body"`
	Title      string `json:"title"`
	Closing    []struct {
		Number int `json:"number"`
	} `json:"closingIssuesReferences"`
	Checks []Check `json:"statusCheckRollup"`
}
type Check struct {
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func (c Check) Key() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
}
func (c Check) Success() bool {
	return c.Conclusion == "SUCCESS" || c.Conclusion == "NEUTRAL" || c.Conclusion == "SKIPPED" || c.State == "SUCCESS"
}
func (c Check) Pending() bool {
	return c.State == "PENDING" || c.Status == "QUEUED" || c.Status == "IN_PROGRESS" || c.Status == "PENDING" || c.Status == "WAITING"
}

const prFields = "number,url,state,headRefOid,headRefName,baseRefName,baseRefOid,mergeStateStatus,mergeable,body,title,closingIssuesReferences,statusCheckRollup"

func (g GH) FindPR(ctx context.Context, p Project, branch string) (*PR, error) {
	b, e := Command(ctx, "", "gh", "pr", "list", "--repo", p.Repo, "--head", branch, "--state", "all", "--json", "number", "--limit", "100")
	if e != nil {
		return nil, e
	}
	var refs []struct {
		Number int `json:"number"`
	}
	if e = json.Unmarshal([]byte(b), &refs); e != nil {
		return nil, e
	}
	if len(refs) == 0 {
		return nil, nil
	}
	b, e = Command(ctx, "", "gh", "pr", "view", strconv.Itoa(refs[0].Number), "--repo", p.Repo, "--json", prFields)
	if e != nil {
		return nil, e
	}
	var pr PR
	e = json.Unmarshal([]byte(b), &pr)
	return &pr, e
}
func (g GH) CreatePR(ctx context.Context, p Project, a Attempt) (*PR, error) {
	body := fmt.Sprintf("Implements issue #%d.\n\nManaged by Changathi. Leave the issue open for human review after merge.", a.Issue.Number)
	_, e := Command(ctx, "", "gh", "pr", "create", "--repo", p.Repo, "--head", a.Branch, "--base", p.Base, "--title", fmt.Sprintf("[Changathi] %s", a.Issue.Title), "--body", body)
	if e != nil {
		return nil, e
	}
	return g.FindPR(ctx, p, a.Branch)
}
func (g GH) Merge(ctx context.Context, p Project, pr *PR, method string) error {
	_, e := Command(ctx, "", "gh", "pr", "merge", strconv.Itoa(pr.Number), "--repo", p.Repo, "--"+method, "--match-head-commit", pr.Head)
	return e
}
func (g GH) MarkReview(ctx context.Context, p Project, n int) error {
	if e := g.api(ctx, "repos/"+p.Repo+"/labels", nil, "--method", "POST", "-f", "name=agent:review", "-f", "color=5319e7"); e != nil { // existing labels are normal; verify rather than suppress unrelated errors
		var label Label
		if check := g.api(ctx, "repos/"+p.Repo+"/labels/agent%3Areview", &label); check != nil {
			return e
		}
	}
	if e := g.api(ctx, fmt.Sprintf("repos/%s/issues/%d/labels", p.Repo, n), nil, "--method", "POST", "-f", "labels[]=agent:review"); e != nil {
		return e
	}
	i, e := g.Issue(ctx, p, n)
	if e != nil {
		return e
	}
	if i.Ready() {
		return g.api(ctx, fmt.Sprintf("repos/%s/issues/%d/labels/agent%%3Aready", p.Repo, n), nil, "--method", "DELETE")
	}
	return nil
}
func (g GH) MutateIssue(ctx context.Context, p Project, n int, action string, fields map[string]any) (any, error) {
	path := fmt.Sprintf("repos/%s/issues", p.Repo)
	method := "PATCH"
	if action == "create" {
		method = "POST"
	} else {
		path += "/" + strconv.Itoa(n)
	}
	args := []string{"--method", method}
	switch action {
	case "close":
		fields = map[string]any{"state": "closed"}
	case "label":
		path += "/labels"
		method = "POST"
		args = []string{"--method", method}
	case "create", "edit":
	default:
		return nil, fmt.Errorf("unknown issue action")
	}
	for k, v := range fields {
		if xs, ok := v.([]any); ok {
			for _, x := range xs {
				args = append(args, "-f", k+"[]="+fmt.Sprint(x))
			}
		} else {
			args = append(args, "-f", k+"="+fmt.Sprint(v))
		}
	}
	var out any
	e := g.api(ctx, path, &out, args...)
	return out, e
}
