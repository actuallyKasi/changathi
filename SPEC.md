# Changathi: Personal Coding Agent Orchestrator

Status: Draft for discussion, 2026-10-08.

Implementation language: Go. Target platforms: macOS and Linux.

## 1. Purpose and scope

Changathi is a single-user service that executes GitHub issues across multiple repositories using coding agents. One daemon manages every registered project on its host. Each project corresponds to exactly one GitHub repository. A Go command-line client provides commands, a live terminal dashboard, and agent messaging.

The initial provider is Codex. A provider adapter boundary permits a later Claude implementation without replacing scheduling, storage, GitHub integration, or the CLI. Claude execution is outside v1.

The service is inspired by [Symphony](https://github.com/openai/symphony/blob/main/SPEC.md), particularly its polling, workspace, and repository workflow concepts. This is an independent design; it adds a multi-project registry, durable state, operator commands, and an explicit GitHub delivery lifecycle.

MUST and SHOULD identify required behavior and recommended behavior respectively. Proposed defaults below are design proposals, not additional user requirements.

## 2. Confirmed requirements

- One service instance manages multiple projects concurrently.
- One repository per project; cross-repository tasks are outside v1.
- Personal use on macOS or Linux; no multi-user authorization system.
- Both automatic issue pickup and explicit CLI dispatch.
- Agents may merge pull requests without human approval.
- Commands, a live terminal dashboard, and messages to agents.
- Codex initially, with an extensible provider interface.
- Git worktrees for per-issue execution.

After an agent merges a PR, the associated GitHub issue MUST remain open for human review and manual closure. Merging closes the PR, not the work item's review lifecycle.

## 3. Deployment and architecture

One binary, `changathi`, supplies daemon and client commands. A daemon runs on either a Mac or Linux host, with all agents and worktrees on that host. It MUST continue working when a client exits.

The initial transport is a versioned local API over a Unix domain socket restricted to the owning OS user. A CLI on another machine may invoke the remote CLI through SSH; a direct remote client transport is deferred. macOS launchd and Linux systemd service examples SHOULD be supplied.

Components:

| Component | Responsibility |
| --- | --- |
| Project registry | Repository identities, enabled status, configuration |
| GitHub adapter | Issues, labels, PRs, checks, reconciliation, mutations |
| Scheduler | Eligibility, claims, fairness, retries, account capacity |
| Workspace manager | Managed clones, branches, worktrees, retention |
| Agent runner | Provider sessions, turns, messages, events, interruption |
| Delivery controller | Validated PR publication and merge operations |
| State store | SQLite records, durable messages, operation journal |
| Local API | Shared command/dashboard interface and event streaming |

The daemon MUST be the sole writer to its database. It MUST acquire an exclusive instance lock before dispatching. SQLite data and workspaces MUST reside on local storage. Running two independent daemons against the same repositories is unsupported in v1 and MUST be documented during setup.

## 4. Data model and persistence

- Project: UUID, unique short name, GitHub host and immutable repository ID, owner/name, base branch, configuration, enabled status.
- Work item: UUID, project ID, GitHub issue ID and number, source snapshot, execution generation, lifecycle state, associated PR, timestamps.
- Attempt: UUID, work item ID, attempt number, provider, model, session/thread reference, workspace, configuration revision, status, failure reason, timing and usage.
- Message: UUID, attempt ID, text, delivery state, provider turn reference, timestamps.
- Event: monotonic sequence ID, project/work item/attempt references, timestamp, event type, structured payload.
- Operation: idempotency key, intended external action, target, result, reconciliation status.

Repository ID plus issue ID defines issue identity; issue number alone MUST NOT identify work across projects. Only one execution generation per issue may be active.

SQLite persists claims, attempts, queued messages, outcomes, and external operation intent. Large logs live in permission-restricted files referenced from SQLite. Secrets MUST NOT appear in database records, generated prompts, or logs.

## 5. Configuration

Service configuration holds project registration, data paths, global limits, and credential references. Each repository may provide `WORKFLOW.md` with YAML settings and a Markdown prompt. It defines instructions, validation commands, timeouts, and project concurrency.

Precedence: built-in defaults, service defaults, repository workflow, explicit project overrides. Host-enforced limits and credential boundaries cannot be increased by repository configuration. A run snapshots its resolved configuration and workflow revision.

Workflow files MUST be loaded from the configured trusted base revision, never automatically from agent edits in an issue branch. Invalid configuration blocks new dispatch for that project and appears in status; other projects continue. Reloads affect future attempts; disabling dispatch also prevents new turns after current work reaches a safe stop.

Proposed defaults: global concurrency 2, per-project concurrency 1, poll interval 30 seconds, three transient retries with exponential backoff and jitter. These values MUST be configurable.

## 6. Scheduling and GitHub issues

Automatic pickup selects open issues with `agent:ready` in enabled projects. Explicit `issue run` dispatches a specified open issue without requiring that label. Every dispatched item records its trigger; removing the label cancels eligibility only for automatic work.

The daemon MUST re-fetch issue eligibility before dispatch and before starting delivery operations. Closed issues stop further execution. CLI cancellation prevents redispatch until an explicit retry or a new ready-label activation. User messages and edits to issue text do not implicitly create new attempts.

Use round-robin selection between eligible projects, then configured issue priority and oldest readiness time within a project. Global and per-project capacity both apply. Retry waits, CI waits, and account-limit waits do not occupy an agent execution slot.

GitHub polling MUST paginate, respect rate limits, and back off on failures. Webhooks are deferred. GitHub is authoritative for issue openness, PR state, and checks; SQLite is authoritative for local execution state and operator commands.

Authentication initially uses the operator's GitHub CLI credential or an explicit token reference. Startup diagnostics verify access. Necessary permissions depend on private repositories and requested issue/PR operations; credentials are never copied into worktrees.

## 7. Delivery lifecycle and autonomous merge

Execution states:

`queued -> preparing -> running -> validating -> pr_open -> waiting_checks -> merging -> awaiting_review -> done`

Alternate states: `retry_wait`, `waiting_account`, `needs_attention`, `cancelled`. Waiting states MUST include an explicit reason and any known wake condition. A completed agent turn does not by itself mean the issue is delivered.

The agent implements changes, runs project validation, publishes a branch, opens a PR, and addresses CI failures. The delivery controller performs agent-requested merge operations under the configured policy. No separate human merge approval is required.

Before merging, the controller MUST verify:

- The PR belongs to the expected repository and managed branch.
- The issue is still eligible and the work item is not cancelled.
- Validation succeeded for the current PR head SHA.
- Configured and repository-required checks succeeded for that SHA.
- GitHub reports that the PR can merge under repository rules.

Merge MUST be conditional on the expected head SHA. Concurrent merges into a project's base branch MUST be serialized locally; a changed base requires applicable validation/checks to be refreshed. Missing or indeterminate check information MUST NOT be treated as success. Repositories without CI use explicitly configured local validation commands.

Existing branch protection is respected. If required human reviews prevent autonomous merging, status becomes `needs_attention`; Changathi does not bypass those rules. Proposed default merge method: squash, configurable per repository.

After merge, transition to `awaiting_review`, retain the issue open, remove readiness, and add `agent:review`. The agent and delivery controller MUST avoid issue-closing keywords and automatic closing associations in managed PRs and commits. The controller MUST check those associations before merge. The human later closes the issue, which transitions local state to `done`. If an external automation unexpectedly closes the issue during merge, surface the discrepancy for the operator; do not silently treat it as human review or reopen a deliberately closed issue.

An issue awaiting review MUST NOT be automatically dispatched again. A CLI retry/rework action creates a new execution generation and a new branch/PR when the previous PR was already merged. Conflicts or repeated failing validation become `needs_attention` after the configured limit.

## 8. Workspaces

Changathi maintains its own clone of each repository, separate from the user's working checkout. Each execution generation receives a deterministic branch and Git worktree under the service data directory, keyed by stable project and work item identifiers.

Creation, fetch, and worktree administration MUST be serialized where shared Git metadata requires it. Agents run with their worktree as the working directory. Retries reuse the worktree when consistent with recorded state. Recovery MUST preserve uncommitted work.

Worktrees isolate Git changes, not processes or filesystem permissions. Agents operate as the daemon's OS user. Container isolation is outside v1. Agent subprocess environments SHOULD contain only the credentials and variables required by the selected tools.

Cleanup MUST never remove active worktrees or unpreserved changes. Proposed retention: retain completed worktrees for seven days; keep failed worktrees until explicitly cleaned. Cleanup previews affected paths and uses Git-aware removal.

## 9. Agent provider boundary

The Go runner interface MUST support capability discovery, session creation/resumption, starting a turn, streaming normalized events, interruption, and session shutdown. Messaging supports steering an active turn when available, or queueing for the next turn otherwise. The CLI MUST show which happened.

Normalized events include session started, message delta, tool activity, turn completed/failed, usage update, account limitation, and input required. Provider-specific metadata can be preserved without driving core scheduling logic.

Codex v1 uses a supervised `codex app-server` subprocess over stdio. The adapter owns protocol initialization, request IDs, bounded event buffering, stderr capture, and graceful termination. Persist provider session references for recovery. Target a tested Codex version and validate integration against its generated protocol schema.

Prefer one app-server process per active attempt for process failure isolation. A provider/account coordinator combines reported capacity and prevents dispatch bursts when limits are reached. An idle CI wait does not require a live agent process; the thread can be resumed for subsequent work.

## 10. CLI and terminal dashboard

Proposed command surface:

```text
changathi daemon start|status|stop
changathi doctor
changathi project add <name> --repo <owner/repo>
changathi project list|show|pause|resume
changathi issue list --project <name>|--all
changathi issue show <project>#<number>
changathi issue create --project <name> --title ... --body-file ...
changathi issue edit <project>#<number> --body-file ...
changathi issue label <project>#<number> --add agent:ready
changathi issue run|retry|cancel <project>#<number>
changathi issue close <project>#<number>
changathi agent list
changathi agent show <attempt-id>
changathi agent logs <attempt-id> --follow
changathi agent message <attempt-id> --text ...
changathi agent stop <attempt-id>
changathi watch
changathi usage
changathi workspace list|clean
```

Commands MUST support explicit project selection, stable exit codes, and `--json` for machine-readable results where meaningful. Mutating commands use request IDs so transport retries do not duplicate actions. CLI issue closure is an explicit human operation, separate from autonomous merge.

The dashboard shows project queues, active agents, latest activity, PR/check state, messages awaiting delivery, failures, and reported account limits. It consumes the same API as commands. Disconnecting or quitting it does not cancel work. Event streams support reconnection from a sequence cursor or an explicit full refresh if history expired.

Messages are durably queued before acknowledgment. Delivery records distinguish queued, sent, acknowledged, and uncertain. On ambiguous provider failure, do not silently duplicate steering messages. The operator can inspect uncertainty and resend deliberately.

## 11. Recovery and operational behavior

On startup, the daemon reconciles recorded attempts, managed worktrees, provider sessions, issues, and PRs before dispatching replacements. It MUST NOT assume every formerly running attempt failed, or that a timed-out merge did not happen.

External actions use journaled intent and remote reconciliation. Before retrying PR creation, find an existing PR for the managed branch. Before retrying a merge, check the PR's merged state. Do not promise exactly-once remote effects across crashes; provide duplicate-resistant operations and explicit uncertain outcomes.

Transient network/provider failures use bounded retries. Authentication errors, invalid configuration, missing permissions, and exhausted validation retries require attention. Rate limits have their own wait path and do not consume ordinary transient retries.

Graceful shutdown disables dispatch, requests provider interruption, records outstanding work, and terminates child process groups after a configurable deadline. Restart MUST ensure an old worker is no longer active before resuming its work item.

Structured logs and CLI diagnostics include project, issue, attempt, and operation IDs. Log retention and disk-use limits MUST be configurable; event backpressure must not deadlock provider communication.

## 12. Acceptance criteria

1. One daemon runs work in two repositories with no identity/workspace collisions.
2. Label and manual dispatch racing for one issue produce one active execution.
3. Global and per-project limits hold; one busy project cannot starve another.
4. Closing an issue or cancelling execution prevents later dispatch and merge.
5. Agents publish and merge when configured validation and GitHub rules allow it, without a Changathi human approval step.
6. A changed PR head cannot be merged using checks from a previous head.
7. Restart after PR creation or merge reconciles remote state without duplicate delivery.
8. Account exhaustion preserves work and pauses dispatch without paid fallback.
9. Dashboard disconnection leaves agents running; log/event reconnection is supported.
10. Agent messages expose delivery state and survive daemon restart.
11. Dirty worktrees survive failure and cleanup attempts.
12. Provider contract tests use a fake provider; Codex integration tests exercise the pinned protocol on macOS and Linux.
13. Merging a managed PR leaves its issue open in `awaiting_review`; polling does not redispatch it, and human closure completes the lifecycle.

## 13. Suggested implementation sequence

1. Go binary, configuration, SQLite, daemon socket, project registry, diagnostics.
2. GitHub reading, manual dispatch, managed worktrees, Codex runner and events.
3. Automatic polling, fair scheduling, retries, quota handling, restart recovery.
4. Validation, PR delivery, autonomous merging, confirmed human-review lifecycle.
5. Issue management commands, messaging, dashboard, service packaging.

Before implementation, review the proposed defaults. Direct remote client transport, containers, multi-host execution, multiple users, cross-repository tasks, and Claude execution remain outside v1.

## 14. Supervisor extension

See [SUPERVISOR_SPEC.md](SUPERVISOR_SPEC.md) for the parent-issue supervisor, dependency gates, scheduling, persistence, and implementation acceptance criteria. The deterministic controller is implemented as an extension; the optional advisory agent remains deferred.
