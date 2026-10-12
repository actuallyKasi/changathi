# Changathi: Personal Coding Agent Orchestrator

Status: v1 implemented, 2026-10-12. This document describes the implemented behavior; "Implementation note" paragraphs record where the code narrows or differs from the original 2026-10-08 design.

Implementation language: Go. Target platforms: macOS and Linux.

## 1. Purpose and scope

Changathi is a single-user service that executes GitHub issues across multiple repositories using coding agents. One daemon manages every registered project on its host. Each project corresponds to exactly one GitHub repository. A Go command-line client provides commands. The operator reviews results and asks for rework on GitHub (§7); there is no channel for messaging a running agent. The CLI is designed to be driven by a coding assistant (Claude Code) through a bundled skill, so its read commands return compact, bounded output; that operator assistant is independent of the provider that runs the agents.

The initial provider is Codex. A provider adapter boundary permits a later Claude implementation without replacing scheduling, storage, GitHub integration, or the CLI. Claude execution was outside v1 and is now added by the extension in §15.

The service is inspired by [Symphony](https://github.com/openai/symphony/blob/main/SPEC.md), particularly its polling, workspace, and repository workflow concepts. This is an independent design; it adds a multi-project registry, durable state, operator commands, and an explicit GitHub delivery lifecycle.

MUST and SHOULD identify required behavior and recommended behavior respectively. Defaults below are the values the implementation ships with.

## 2. Confirmed requirements

- One service instance manages multiple projects concurrently.
- One repository per project; cross-repository tasks are outside v1.
- Personal use on macOS or Linux; no multi-user authorization system.
- Both automatic issue pickup and explicit CLI dispatch.
- Pull requests may be merged without human approval (Changathi's delivery controller merges; see §7).
- Commands, an agent-facing skill, and a review loop on GitHub issues.
- Codex initially, with an extensible provider interface.
- Git worktrees for per-issue execution.

After a managed PR is merged, the associated GitHub issue MUST remain open for human review and manual closure. Merging closes the PR, not the work item's review lifecycle.

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
| Agent runner | Provider sessions, turns, events, interruption |
| Delivery controller | Validated PR publication and merge operations |
| State store | SQLite records, operation journal |
| Local API | Command interface and event streaming |

The daemon MUST be the sole writer to its database. It MUST acquire an exclusive instance lock before dispatching. SQLite data and workspaces MUST reside on local storage. Running two independent daemons against the same repositories is unsupported in v1 and MUST be documented during setup.

Implementation note: the API is versioned HTTP (`/v1/status`, `/v1/events`, `/v1/transcript`, `/v1/action`) over `<data_dir>/daemon.sock`, created with mode 0600 inside a 0700 data directory. The instance lock is `<data_dir>/daemon.lock`; it prevents two daemons sharing one data directory, but nothing detects two daemons with different data directories pointing at the same repositories. `changathi daemon start` runs in the foreground; the `packaging/` launchd plist and systemd unit supervise it as a service.

## 4. Data model and persistence

- Project: UUID, unique short name, GitHub host and immutable repository ID, owner/name, base branch, configuration, enabled status.
- Work item: UUID, project ID, GitHub issue ID and number, source snapshot, execution generation, lifecycle state, associated PR, timestamps.
- Attempt: UUID, work item ID, attempt number, provider, model, session/thread reference, workspace, configuration revision, status, failure reason, timing and usage.
- Event: monotonic sequence ID, project/work item/attempt references, timestamp, event type, structured payload.
- Operation: idempotency key, intended external action, target, result, reconciliation status.

Repository ID plus issue ID defines issue identity; issue number alone MUST NOT identify work across projects. Only one execution generation per issue may be active.

SQLite persists claims, attempts, outcomes, and external operation intent. Large logs live in permission-restricted files referenced from SQLite. Secrets MUST NOT appear in database records, generated prompts, or logs.

Implementation note: there is no separate work-item record. Each execution generation is one attempt record carrying the issue snapshot, generation number, trigger, lifecycle state, branch, PR, head/base SHAs, and the resolved workflow; retrying an undelivered generation re-queues that same attempt rather than creating a new one. Records are JSON documents in a `records(kind, id, data)` table in `<data_dir>/state.db` (WAL mode), and events live in an append-only `events` table whose autoincrement `seq` is the stream cursor, trimmed to `event_limit`. Identifiers are random 128-bit hex strings, not UUIDs. Per-attempt logs are `<data_dir>/logs/<attempt-id>.log`, rotated once at `max_log_bytes`. Mutating API requests are recorded by request ID so a retried request returns the cached result instead of acting twice.

## 5. Configuration

Service configuration holds project registration, data paths, global limits, and credential references. Each repository may provide `WORKFLOW.md` with YAML settings and a Markdown prompt. It defines instructions, validation commands, timeouts, and project concurrency.

Precedence: built-in defaults, service defaults, repository workflow, explicit project overrides. Host-enforced limits and credential boundaries cannot be increased by repository configuration. A run snapshots its resolved configuration and workflow revision.

Workflow files MUST be loaded from the configured trusted base revision, never automatically from agent edits in an issue branch. Invalid configuration blocks new dispatch for that project and appears in status; other projects continue. Reloads affect future attempts; disabling dispatch also prevents new turns after current work reaches a safe stop.

Defaults: global concurrency 1 (one provider session at a time, matching a single-session subscription), per-project concurrency 1, poll interval 30 seconds, three transient retries with exponential backoff and jitter. These values MUST be configurable.

Implementation note:

- Service configuration (`examples/config.yaml`) has `data_dir` (default `~/.local/share/changathi`), `concurrency` (1), `poll_seconds` (30), `codex` (binary path), `max_log_bytes` (16 MiB), and `event_limit` (10,000). Credential references are not configurable; see §6.
- Per-project settings are set at registration: `project add --repo owner/name [--auto] [--concurrency N]`. Automatic pickup is off unless `--auto` is given.
- `WORKFLOW.md` front matter accepts exactly `model`, `validation` (at least one command, required), `required_checks`, `merge_method` (`squash` default, `merge`, or `rebase`), `max_turns` (5), `timeout_seconds` (3600), and `concurrency` (1); unknown keys are rejected. The body is a Go template rendered with `.Issue`.
- Effective project concurrency is the lower of the registered project limit and the workflow's `concurrency`, so a repository can reduce but never raise it.
- Transient failures retry up to three times at roughly 10, 20, and 40 seconds plus up to one second of jitter.

## 6. Scheduling and GitHub issues

Automatic pickup selects open issues with `agent:ready` in enabled projects that have automatic pickup turned on. Explicit `issue run` dispatches a specified open issue without requiring that label. Every dispatched item records its trigger; removing the label cancels eligibility only for automatic work.

The daemon MUST re-fetch issue eligibility before dispatch and before starting delivery operations. Closed issues stop further execution. CLI cancellation prevents redispatch until an explicit retry or a new ready-label activation. Issue comments and edits to issue text do not implicitly create new attempts; the operator requests rework explicitly (§7).

Use round-robin selection between eligible projects, then oldest first within a project. Implementation note: "oldest" is issue creation order as returned by GitHub; issue priority and label-applied time are not used. Global and per-project capacity both apply. Retry waits, CI waits, and account-limit waits do not occupy an agent execution slot.

GitHub polling MUST paginate, respect rate limits, and back off on failures. Webhooks are deferred. GitHub is authoritative for issue openness, PR state, and checks; SQLite is authoritative for local execution state and operator commands.

Authentication uses the operator's GitHub CLI (`gh`) credential; every GitHub call goes through `gh`, and only github.com repositories are supported. An explicit token reference is not implemented. Startup diagnostics verify access. Necessary permissions depend on private repositories and requested issue/PR operations; credentials are never copied into worktrees.

## 7. Delivery lifecycle and autonomous merge

Execution states:

`queued -> preparing -> running -> validating -> pr_open -> waiting_checks -> merging -> awaiting_review -> done`

Alternate states: `retry_wait`, `waiting_account`, `needs_attention`, `cancelled`, `superseded`. Waiting states MUST include an explicit reason and any known wake condition. A completed agent turn does not by itself mean the issue is delivered. `superseded` marks an `awaiting_review` generation replaced by a newer rework generation of the same issue.

Responsibilities are split between the agent and the delivery controller:

- The agent implements changes and commits them on the managed branch in its worktree. It is instructed not to push, open or merge PRs, close issues, or use closing keywords, and it may run the validation commands itself.
- After each agent turn, the controller merges the latest base into the branch, runs the workflow's validation commands, and requires a clean worktree, at least one new commit, and no closing keywords in commit messages. A failure becomes the next turn's prompt, up to `max_turns`.
- On success the controller pushes the branch, finds or creates the PR (`[Changathi] <issue title>`), waits for checks and mergeability, and merges. No separate human merge approval is required.
- When a check fails, the controller re-queues the attempt and resumes the same Codex thread with the failure as context. That new turn counts against `max_turns`; after the limit the attempt becomes `needs_attention`. No agent process is kept alive while checks run.

Before merging, the controller MUST verify:

- The PR belongs to the expected repository and managed branch.
- The issue is still eligible and the work item is not cancelled.
- Validation succeeded for the current PR head SHA.
- Configured and repository-required checks succeeded for that SHA.
- GitHub reports that the PR can merge under repository rules.

Merge MUST be conditional on the expected head SHA. Concurrent merges into a project's base branch MUST be serialized locally; a changed base requires applicable validation/checks to be refreshed. Missing or indeterminate check information MUST NOT be treated as success. Local validation commands are always required; repositories without CI rely on them alone by leaving `required_checks` empty.

Implementation note: merges use `gh pr merge --match-head-commit <validated head>`, and a per-project lock serializes delivery. If the PR's head or base SHA no longer matches what was validated, or GitHub reports a conflict, the controller re-validates, pushes, and waits for fresh checks. Any merge state other than `CLEAN` or `HAS_HOOKS` (for example, blocked by required reviews) becomes `needs_attention`.

Existing branch protection is respected. If required human reviews prevent autonomous merging, status becomes `needs_attention`; Changathi does not bypass those rules. Default merge method: squash, configurable per repository.

After merge, transition to `awaiting_review`, retain the issue open, remove readiness, and add `agent:review`. The agent and the delivery controller MUST avoid issue-closing keywords and automatic closing associations in managed PRs and commits. The controller MUST check those associations before merge. The human later closes the issue, which transitions local state to `done`. If an external automation unexpectedly closes the issue during merge, surface the discrepancy for the operator; do not silently treat it as human review or reopen a deliberately closed issue.

An issue awaiting review MUST NOT be automatically dispatched again. Only an explicit operator action, `issue retry` or the `agent:rework` label, reworks it; that creates a new execution generation and a new branch/PR when the previous PR was already merged. Conflicts or repeated failing validation become `needs_attention` after the configured limit.

### Review loop on GitHub

The operator reviews results and drives rework on GitHub, without the CLI:

- **Status comments.** When an issue's latest attempt enters `needs_attention`, Changathi posts the reason, the last failure the agent saw, and the next step as a comment on the issue. When it reaches `awaiting_review`, the comment names the merged PR and the operator's choices. Each attempt has one status comment, edited in place and rewritten only when its content changes; a failed write is retried on the next poll. Failure text is posted as is, so it is visible to anyone who can read the issue.
- **Rework trigger.** Adding the `agent:rework` label to an open issue in an enabled project requests rework, whether or not automatic pickup is on. Changathi removes the label before acting, so a lost removal cannot start rework twice, then re-queues exactly as `issue retry` does: the same generation (branch, worktree, open PR) when nothing merged, or a new generation that supersedes an `awaiting_review` one. A new generation also removes `agent:review`. A label added while the issue's attempt is still working takes effect once it stops. A refused request (for example a supervisor gate) is answered with a comment giving the reason.
- **Operator comments in the prompt.** Rework captures the operator's issue comments created since the previous capture (or since the previous attempt was created) and adds them to the implement prompt as the operator's instructions. Only comments by the authenticated `gh` user with a `User` account are trusted; anyone else's comment could inject instructions. Changathi posts with the same login, so its own comments carry a hidden `<!-- changathi` marker and are skipped, as are `/approve-evidence` approvals. The newest comments are kept when they exceed the prompt budget (8 KiB each, 24 KiB in all). Comments are captured when rework is requested, not re-read during the run; edits to a comment after capture are ignored.
- **Answer-only rework.** A rework prompt with operator comments tells the agent that a question, or a comment that needs no code change, is answered with no edit or commit. The agent ends its final message with one ```` ```changathi-reply ```` fenced block. If HEAD and the worktree are unchanged after the turn, Changathi records a `reply` event and stores the answer on the attempt as `Reply`. It skips validation, verification and delivery. If the attempt has no PR or commits of its own and the generation before it merged, the attempt takes over that generation's PR, head and base SHA, regains `agent:review` and returns to `awaiting_review`. Otherwise it goes to `needs_attention`. The answer is posted as its own issue comment, keyed by the attempt and a digest of the comment body, so a later answer never overwrites an earlier one. A turn that changed the branch is validated as usual and its reply block is ignored. When the no-commit validation check fails on a rework with comments, the retry prompt reminds the agent of the reply block instead of asking only for a commit. A question that needs no code change does not need rework at all.
- **Follow-ups.** A separate bug or idea found during review belongs in a new issue; the reviewed issue is closed as completed when its own criteria are met.

Implementation note: the review loop needs the adapter's optional `core.Feedback` interface (the `gh` adapter implements it). Without it, status stays local and rework needs `issue retry`. Rework records trigger `rework` for the label and `manual` for `issue retry`.

## 8. Workspaces

Changathi maintains its own clone of each repository, separate from the user's working checkout. Each execution generation receives a deterministic branch and Git worktree under the service data directory, keyed by stable project and work item identifiers.

Creation, fetch, and worktree administration MUST be serialized where shared Git metadata requires it. Agents run with their worktree as the working directory. Retries reuse the worktree when consistent with recorded state. Recovery MUST preserve uncommitted work.

Worktrees isolate Git changes, not processes or filesystem permissions. Agents operate as the daemon's OS user. Container isolation is outside v1. Agent subprocess environments SHOULD contain only the credentials and variables required by the selected tools.

Cleanup MUST never remove active worktrees or unpreserved changes. Cleanup previews affected paths and uses Git-aware removal.

Implementation note:

- The managed clone is `<data_dir>/repos/<project-id>`, and worktrees are `<data_dir>/worktrees/<project-id>/<attempt-id>`.
- Branches are `changathi/<first 8 chars of project-id>/<issue number>/g<generation>`.
- There is no time-based retention; nothing is removed automatically. `workspace clean <agent-id>` previews by default, and `--apply` runs `git worktree remove`.
- Only clean worktrees of delivered work (`done`, `awaiting_review`, `superseded`) can be removed. Failed and cancelled worktrees are always preserved, and the CLI cannot remove them.

## 9. Agent provider boundary

The Go runner interface MUST support capability discovery, session creation/resumption, starting a turn, streaming normalized events, interruption, and session shutdown.

Normalized events include session started, message delta, tool activity, turn completed/failed, usage update, account limitation, and input required. Provider-specific metadata can be preserved without driving core scheduling logic.

Codex v1 uses a supervised `codex app-server` subprocess over stdio. The adapter owns protocol initialization, request IDs, bounded event buffering, stderr capture, and graceful termination. Persist provider session references for recovery. Target a tested Codex version and validate integration against its generated protocol schema.

Prefer one app-server process per active attempt for process failure isolation. A provider/account coordinator combines reported capacity and prevents dispatch bursts when limits are reached. An idle CI wait does not require a live agent process; the thread can be resumed for subsequent work.

Implementation note:

- The Go interface is narrower than described above: `Provider.Open` returns a `Session` with `Start`, which starts or resumes a thread in the worktree, plus `Run` for one turn with streamed events, and `Close`. There is no capability discovery; interruption is context cancellation.
- Each attempt gets its own `codex app-server` process with a reduced environment.
- An account limit is recognized from provider error text ("rate limit", "usage limit", or "quota"). It sets a provider-wide gate that pauses all dispatch for five minutes and puts the attempt in `waiting_account`.

## 10. CLI and skill

The CLI's primary user is a coding assistant, so read commands default to compact, bounded output and never dump unbounded history. Implemented command surface (all commands accept `--json`):

```text
changathi daemon start|status|stop
changathi doctor
changathi summary
changathi project add <name> --repo <owner/repo> [--auto] [--concurrency N]
changathi project list|pause|resume
changathi issue list --project <name>|--all
changathi issue show <project>#<number>
changathi issue run|retry|cancel <project>#<number>
changathi agent list [--active] [--project <name>] [--limit N]
changathi agent show <attempt-id> [--full]
changathi agent logs <attempt-id> [--tail N] [--last] [--full] [--follow] [--raw]
changathi agent stop <attempt-id>
changathi supervisor ...            (see SUPERVISOR_SPEC.md)
changathi usage
changathi workspace clean <attempt-id> [--apply]
changathi skill install [--dir <repo>] [--print]
```

`summary` reports, in one call, the running agents, the newest attempt for each issue that needs attention or awaits review, active supervisors, paused projects, and any provider pause. `agent list` returns compact rows (id, issue, title, state, PR, one-line detail), newest first, skips superseded generations, and defaults to 20 rows. `agent show` omits the recorded workflow and issue body unless `--full` is given. `agent logs` prints plain text; tool and diff output appears only for failed items unless `--full` is given, and `--tail`/`--last` bound the window. `daemon status --json` remains the full snapshot for debugging.

`project add` also creates the `agent:ready`, `agent:review`, `agent:rework`, and `supervisor` labels and warns when `WORKFLOW.md` is missing from the base branch. Creating, editing, labelling, and closing issues are GitHub operations done with `gh`; Changathi does not wrap them. Issue closure remains an explicit human operation, separate from autonomous merge.

`skill install` writes the skill embedded in the binary (`skills/changathi/SKILL.md`) to `~/.claude/skills/changathi/SKILL.md`, or under `<repo>/.claude/skills/` with `--dir`, so the installed skill always matches the CLI. The skill covers onboarding a repository, writing issues agents can finish, supervisor plans, and checking on and reworking agents. `doctor` checks that `git`, `gh`, and Codex are on the path and runs `gh auth status` and `codex login status`.

Commands MUST support explicit project selection, stable exit codes, and `--json` for machine-readable results where meaningful. Mutating commands use request IDs so transport retries do not duplicate actions. Event streams support reconnection from a sequence cursor or an explicit full refresh if history expired.

## 11. Recovery and operational behavior

On startup, the daemon reconciles recorded attempts, managed worktrees, provider sessions, issues, and PRs before dispatching replacements. It MUST NOT assume every formerly running attempt failed, or that a timed-out merge did not happen.

External actions use journaled intent and remote reconciliation. Before retrying PR creation, find an existing PR for the managed branch. Before retrying a merge, check the PR's merged state. Do not promise exactly-once remote effects across crashes; provide duplicate-resistant operations and explicit uncertain outcomes.

Transient network/provider failures use bounded retries. Authentication errors, invalid configuration, missing permissions, and exhausted validation retries require attention. Rate limits have their own wait path and do not consume ordinary transient retries.

Graceful shutdown disables dispatch, requests provider interruption, records outstanding work, and terminates child process groups after a configurable deadline. Restart MUST ensure an old worker is no longer active before resuming its work item.

Implementation note:

- On startup, attempts left in `preparing`, `running`, `validating`, `pr_open`, or `merging` move to `retry_wait`, but only after their per-attempt execution lock (under `<data_dir>`) can be taken. If the lock is still held, the attempt becomes `needs_attention`.
- Every resumed attempt checks GitHub for its branch's PR before doing anything else. A merged PR goes straight to the review step, and a PR closed without merging stops the attempt.
- PR creation and merge are journaled as operation records (`intent`, then `complete` or `uncertain`).
- A merge call that errors is followed by a PR state check, so a merge that succeeded despite the error is still recognized.

Structured logs and CLI diagnostics include project, issue, attempt, and operation IDs. Log retention and disk-use limits MUST be configurable; event backpressure must not deadlock provider communication.

## 12. Acceptance criteria

1. One daemon runs work in two repositories with no identity/workspace collisions.
2. Label and manual dispatch racing for one issue produce one active execution.
3. Global and per-project limits hold; one busy project cannot starve another.
4. Closing an issue or cancelling execution prevents later dispatch and merge.
5. Changathi publishes and merges agent work when configured validation and GitHub rules allow it, without a human approval step.
6. A changed PR head cannot be merged using checks from a previous head.
7. Restart after PR creation or merge reconciles remote state without duplicate delivery.
8. Account exhaustion preserves work and pauses dispatch without paid fallback.
9. CLI disconnection leaves agents running; log/event reconnection is supported.
10. A stopped or delivered attempt is explained on its GitHub issue, and `agent:rework` plus operator comments rework it without the CLI; other users' comments never reach the agent.
11. Dirty worktrees survive failure and cleanup attempts.
12. Provider contract tests use a fake provider; Codex integration tests exercise the pinned protocol on macOS and Linux.
13. Merging a managed PR leaves its issue open in `awaiting_review`; polling does not redispatch it, and human closure completes the lifecycle.

## 13. Implementation sequence (completed)

1. Go binary, configuration, SQLite, daemon socket, project registry, diagnostics.
2. GitHub reading, manual dispatch, managed worktrees, Codex runner and events.
3. Automatic polling, fair scheduling, retries, quota handling, restart recovery.
4. Validation, PR delivery, autonomous merging, confirmed human-review lifecycle.
5. Agent-facing commands and skill, service packaging.

All five stages are implemented. Direct remote client transport, GitHub webhooks, token-reference authentication, GitHub Enterprise hosts, time-based worktree retention, containers, multi-host execution, multiple users, and cross-repository tasks remain outside v1. Claude execution was added afterwards (§15).

## 14. Supervisor extension

See [SUPERVISOR_SPEC.md](SUPERVISOR_SPEC.md) for the parent-issue supervisor, dependency gates, scheduling, persistence, and implementation acceptance criteria. The deterministic controller is implemented as an extension; the optional advisory agent remains deferred.

## 15. Claude provider extension

See [CLAUDE_PROVIDER_SPEC.md](CLAUDE_PROVIDER_SPEC.md) for the Claude provider adapter, per-role models (for example Opus to implement and Sonnet to verify), and the verification step between validation and publication. It is implemented as an extension; Codex-only workflows are unchanged.
