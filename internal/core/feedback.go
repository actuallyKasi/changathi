package core

import "context"

// CommentMarker prefixes the hidden marker on every comment Changathi writes.
// Changathi comments with the operator's own credential, so the marker, not the
// author, tells its comments apart from the operator's review feedback.
const CommentMarker = "<!-- changathi"

// ReworkLabel is the operator's GitHub-side request to rework an issue.
const ReworkLabel = "agent:rework"

// Feedback is the GitHub side of the review loop: status comments on issues,
// the agent:rework trigger, and the operator's comments for rework prompts.
// It is optional so older adapters remain compatible; without it, failures stay
// local and rework needs `issue retry`.
type Feedback interface {
	Operator(context.Context) (string, error)
	EvidenceComments(context.Context, Project, int) ([]EvidenceComment, error)
	Labeled(context.Context, Project, string) ([]Issue, error)
	RemoveLabel(context.Context, Project, int, string) error
	StatusComment(context.Context, Project, int, string, string) error
}

// StatusComment creates or edits Changathi's status comment for one attempt.
func (g GH) StatusComment(ctx context.Context, p Project, n int, id, body string) error {
	_, err := g.upsertComment(ctx, p, n, CommentMarker+"-status:"+id+" -->", body)
	return err
}
