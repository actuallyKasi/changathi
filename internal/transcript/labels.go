package transcript

import "strings"

var stateLabels = map[string]string{
	"done": "Work finished", "completed": "Completed", "pending": "Not started",
	"open": "Issue open", "closed": "Issue closed", "superseded": "Replaced",
	"awaiting_review": "Ready for review", "waiting_review": "Waiting for review",
	"awaiting_parent_review": "Review parent issue", "needs_attention": "Needs attention",
	"retry_wait": "Retry scheduled", "waiting_account": "Waiting for account",
	"waiting_checks": "Waiting for checks", "waiting_capacity": "Waiting for capacity",
	"waiting_remote": "Waiting for GitHub", "waiting_child": "Waiting for children",
	"running": "Working", "preparing": "Preparing", "validating": "Validating",
	"pr_open": "Publishing PR", "merging": "Merging", "active": "Coordinating",
	"queued": "Queued", "cancelled": "Stopped", "paused": "Paused", "failed": "Failed",
	"agent_active": "Agent active", "agent_inactive": "Agent inactive",
	"agent_unknown": "Agent status unknown", "no_agent": "No agent yet",
}

// StateLabel is the user-facing name for a daemon or supervisor state.
func StateLabel(state string) string {
	if label, ok := stateLabels[state]; ok {
		return label
	}
	return strings.ReplaceAll(state, "_", " ")
}
