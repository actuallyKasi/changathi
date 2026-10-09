# Changathi

A personal coding-agent orchestrator written in Go. One daemon runs GitHub issues across multiple repositories, with isolated Git worktrees, a terminal dashboard, and messages to running Codex agents.

Agents implement and commit changes. Changathi validates those commits, publishes PRs, and merges when checks and repository rules permit. The GitHub issue stays open with `agent:review` until you review and close it.

## Build and setup

Requires Go 1.26+, Git, GitHub CLI, and Codex CLI. The Codex protocol handshake was verified with `codex-cli 0.160.0`.

```sh
go build -o bin/changathi ./cmd/changathi
gh auth login
codex login
./bin/changathi doctor
```

Authenticate as the OS user that will run the daemon. Worktrees provide Git isolation; Codex runs unattended as that user with `approvalPolicy=never` and `sandbox=danger-full-access`. Register repositories whose code and workflow commands you trust. Credentials are not copied into worktrees, and provider subprocesses receive an allowlisted environment.

Add a `WORKFLOW.md` to the repository's default branch before dispatching:

```yaml
---
validation:
  - go test ./...
  - go vet ./...
required_checks: []
merge_method: squash
max_turns: 5
timeout_seconds: 3600
concurrency: 1
---
Implement issue #{{.Issue.Number}}: {{.Issue.Title}}.
Follow AGENTS.md and existing repository conventions.
```

Validation is required. Templates use Go `text/template` syntax, with `.Issue` containing the issue's fields. The workflow comes from the trusted base branch and is recorded per execution. Editing the workflow in an agent branch does not change its execution policy.

## Run

Start the daemon in one terminal; it stays in the foreground for service supervision:

```sh
./bin/changathi daemon start
```

Use another terminal:

```sh
./bin/changathi project add backend --repo owner/backend --auto
./bin/changathi project add frontend --repo owner/frontend
./bin/changathi project list
./bin/changathi issue list --all
./bin/changathi issue run 'frontend#42'
./bin/changathi watch
```

`--auto` enables pickup of open issues labelled `agent:ready` and supervisor parents labelled `supervisor`. Projects without it run only on explicit dispatch. Create that readiness label in GitHub before applying it. Closing an issue or removing readiness from automatically dispatched work stops further execution. Cancelling an automatic issue suppresses redispatch until readiness is removed and added again, or you explicitly retry.

```sh
./bin/changathi agent list
./bin/changathi agent logs AGENT_ID --follow
./bin/changathi agent message AGENT_ID --text 'Also cover the empty-input case'
./bin/changathi issue cancel 'backend#7'
./bin/changathi issue retry 'backend#7'
./bin/changathi project pause backend
./bin/changathi project resume backend
./bin/changathi usage
./bin/changathi issue close 'backend#7'
./bin/changathi daemon stop
```

Messages steer an active turn or queue for the next turn. Delivery uncertainty is visible in `daemon status --json`; uncertain messages are not automatically resent. A retry of unfinished work preserves its worktree and Codex thread. When a new generation replaces a pending review, the old attempt becomes `superseded`; its logs, PR, and worktree remain available. Existing stale review rows are repaired when the daemon opens its store. Rework after a merged PR creates a new generation and branch. GitHub branch protection and required review rules still apply; a blocked merge is reported as `needs_attention`.

Issue creation, editing, and labels are also available:

```sh
./bin/changathi issue create --project backend --title 'Handle empty input' --body-file issue.md
./bin/changathi issue edit 'backend#7' --body-file updated-issue.md
./bin/changathi issue label 'backend#7' --add agent:ready
```

All commands support `--help`. `--json` provides structured output. The interactive dashboard refreshes status every two seconds and follows agent output between refreshes; quitting it leaves agents running. For a Linux host, execute its CLI through SSH to connect to that host's daemon.

## Interactive agent output

```sh
./bin/changathi watch
./bin/changathi agent view AGENT_ID
./bin/changathi agent logs AGENT_ID --follow
./bin/changathi agent logs AGENT_ID --raw
./bin/changathi agent logs AGENT_ID --json
```

`watch` opens an interactive dashboard with projects, agent states, and supervisor children. `agent view` opens one transcript directly. Assistant messages render Markdown and highlighted code; commands, diffs, and tool results remain separate. Tool results collapse by default, while failed results expand. Scrolling up pauses live follow; new output does not move your position.

| Key | Action |
| --- | --- |
| Tab | Switch between agent navigation and transcript |
| ↑/↓ or j/k | Navigate agents or scroll output |
| Enter | Open agent or expand/collapse selected tool |
| [ / ] | Select previous/next transcript item |
| / | Filter the transcript; Esc clears the filter |
| p | Load older retained output |
| f or End | Return to live output |
| m | Compose a message; Ctrl-S sends, Esc preserves a draft |
| r | Inspect raw events |
| e | Export loaded transcript to a local temporary Markdown file |
| s / t | Stop the selected agent / retry its issue; Enter confirms, Esc cancels |
| ? | Show keyboard help |
| q | Quit the viewer; daemon work continues |

The composer displays the daemon's actual delivery state. A failed or uncertain submission preserves its text. Retrying the same uncertain submission reuses its request ID; changing that submission requires inspecting delivery first. Drafts persist while navigating within the current viewer, not across viewer restarts.

`--plain` or `NO_COLOR=1` disables colors. Pipes and `TERM=dumb` use ordinary text output; `--json` exposes machine-readable events. The append-only `logs --follow` view prints completed message/tool blocks; use the interactive viewer to see messages while they stream. Events recorded before structured transcripts were introduced retain their original event boundaries.

The viewer loads recent history first and reports retention gaps. Resident history and rendered items are bounded; load older output with `p`, and use `--raw` for provider details. Export includes the loaded transcript, not every historical provider event. The client reconnects when the daemon becomes available again. Restart an existing daemon after rebuilding to enable the new transcript API.

## Configuration and state

Defaults: two concurrent agents globally, one per project, and a 30-second poll interval. Pass daemon flags or a YAML configuration:

```sh
./bin/changathi --config examples/config.yaml daemon start
```

The default state directory is `~/.local/share/changathi`, containing SQLite state, managed repository clones, worktrees, logs, and a Unix socket accessible only to its owner. Each agent log rotates at 16 MiB, and the event feed retains roughly 10,000 events; both limits are configurable. CLI and daemon must use the same `--data-dir`. Do not run independent daemons against the same repositories or share a database over a network filesystem.

Worktrees are preserved after failure. Preview cleanup before removing a clean delivered worktree:

```sh
./bin/changathi workspace clean AGENT_ID
./bin/changathi workspace clean AGENT_ID --apply
```

Service templates are in `packaging/`; replace executable paths, username, and PATH before installing them. They are examples and are not installed by building the project.

## Verification

```sh
go test -race ./...
go vet ./...
CHANGATHI_CODEX_SMOKE=1 go test ./internal/provider -run TestInstalledCodexHandshake -v
```

Tests cover durable claims, worktree/session reuse, two-repository delivery with a fake provider, merge preconditions, cancellation, messaging, request idempotency, and subprocess protocol framing. The opt-in Codex smoke test initializes app-server and creates a local thread without running a model turn. Automated tests do not create real GitHub PRs or merge repositories.

## Current boundaries

GitHub.com is the tracker; GitHub Enterprise and Claude are future adapters. The terminal dashboard supports readable streaming transcripts, collapsible tools, search, and a message composer. Workers inherit an execution lock so a daemon restart cannot replace a surviving agent or validation process. If that lock is still held, inspect the previous execution and retry after it exits. Local GitHub authentication is used through `gh`.

The broader target design is in [SPEC.md](SPEC.md). Project-specific model, workflow validation, required checks, merge method, and timeouts are supported. Advanced priority policies, complete remote-effect reconciliation after ambiguous issue mutations, automatic worktree retention, and direct remote socket access remain follow-up work.

The supervisor design and implementation contract are in [SUPERVISOR_SPEC.md](SUPERVISOR_SPEC.md).

## Supervising dependent issues

Create a parent GitHub issue with the `supervisor` label and a structured plan, using [the homelab example](examples/supervisor-issue.md). References are child issue numbers in the same repository. A supervisor is daemon coordination logic and does not occupy an implementation-agent slot, so project concurrency can remain one. It starts one child and waits for its human completion before advancing.

For a project registered without `--auto`, explicitly activate the parent:

```sh
./bin/changathi supervisor validate 'homelab#PARENT'
./bin/changathi supervisor start 'homelab#PARENT'
./bin/changathi supervisor show 'homelab#PARENT'
./bin/changathi supervisor list --project homelab
```

Replace `PARENT` with the new parent issue number. Validation is read-only; start enrolls the plan and queues its first eligible child. Existing child issues do not need `agent:ready`. Parent and enrolled-child dispatch cannot bypass the supervisor through ordinary `issue run` or automatic labels. The dashboard and a single parent issue comment report progress.

The default `merged_and_closed` gate requires the managed PR to be merged into the project base and the child issue closed as completed by the authenticated GitHub operator after delivery. Merged PRs with open issues wait for human review. Closed-as-not-planned issues and reopened prerequisites block advancement. `completion: closed_completed` is a manual gate with no code worker; use it for non-code tasks that a human completes directly.

For operational work, set `requires_evidence: true` on the child. Changathi posts a review comment on the child issue and a matching action on the supervisor issue, with an exact approval command containing the delivered PR number and evidence URL. A single changed report under `docs/validation/` gets a link pinned to the delivered commit; otherwise the link opens the PR for review.

Review the evidence, close the child as completed, then copy the exact `/approve-evidence #PR HTTPS_URL` command into a new comment on that child. The authenticated human GitHub operator must post it after closure. The daemon validates author, unedited comment, current PR, merge/base/head, and closure before advancing automatically. Other authors, bots, edited comments, old closures, and previous PR approvals cannot release work. Deleting or editing a used GitHub approval invalidates it on the next reconciliation. Explicit pauses and cancellation still require resume. No model call is used to interpret approval.

CLI acknowledgement remains available as a fallback:

```sh
./bin/changathi supervisor reconcile 'homelab#PARENT' --child 4 \
  --ack-completion --evidence https://github.com/OWNER/REPO/issues/4#issuecomment-EXAMPLE
./bin/changathi supervisor resume 'homelab#PARENT'
```

The evidence URL is an operator attestation, not an automated interpretation of a report. Use this explicit acknowledgement for ambiguous/bot/automatic closure too; it cannot bypass a missing merge or an issue that remains open. For previously completed work outside Changathi, first associate its merged PR with `supervisor reconcile 'homelab#PARENT' --child 3 --pr NUMBER`. The adapter verifies repository, base, and merge state. Acknowledgements are bound to the issue closure/update time and delivery head, so later changes require review again.

### Add a child to an existing supervisor

1. Pause the parent with `supervisor pause 'homelab#PARENT'` and let any worker stop.
2. Create a normal issue with `issue create --project homelab --title 'Next task' --body-file child.md` (or use GitHub).
3. Edit the parent's `changathi-supervisor` block to include the new issue and its dependencies. For example, append `issue: 10`, `depends_on: [9]`, and `completion: merged_and_closed` under `children`. A GitHub sub-issue relationship alone does not enroll work.
4. Preview and adopt the new revision, then resume:

```sh
./bin/changathi supervisor reload 'homelab#PARENT'
./bin/changathi supervisor reload 'homelab#PARENT' --apply
./bin/changathi supervisor resume 'homelab#PARENT'
```

A reload remains paused until resume. Changes to a dispatched child's policy/dependencies and removal of dispatched children are rejected; append new work while retaining the existing graph. Changing a parent graph without reloading blocks new work. Removing/re-adding the label cannot undo an operator pause. Cancelled plans require an explicit reload and resume. Child implementation failures are retried with `issue retry 'homelab#CHILD'`, which still checks the parent and dependencies.

All children complete leaves the parent awaiting review; close it as completed after review. The supervisor never closes parent or child issues. Automatic discovery only runs in projects with `auto: true`. No supervisor model sessions are used; the optional advisory LLM described in the spec is deferred.

After rebuilding, restart an existing daemon to load new code. Restart an existing `watch` client to display supervisors. Persistent projects, attempts, ownership, pauses, and revisions are retained in the same data directory.
