# Changathi Claude provider and model roles specification

Status: implemented, 2026-10-12, against Claude Code 2.1.296. Extends [SPEC.md](SPEC.md). MUST denotes a requirement; SHOULD denotes a recommendation. §12 records where the implementation narrows or differs from this design.

## 1. Purpose

Add Claude as a second agent provider beside Codex, and let one attempt use more than one model. A strong model does the open-ended implementation work; a cheaper model independently verifies the result before Changathi publishes it.

Default role assignment for the Claude provider:

| Role | Model | Work |
| --- | --- | --- |
| `implement` | `claude-opus-5-5` (Opus) | Implementation turns, fix-up turns after validation, check, or verification failure |
| `verify` | `claude-sonnet-5-5` (Sonnet) | Read-only review of the validated diff against the issue's acceptance criteria |

Verification adds a judgment gate; it does not replace the deterministic gates in SPEC.md §7. Validation commands, required checks, head-SHA-conditional merge, and the human-review lifecycle are unchanged.

## 2. Scope

In scope:

- A `claude` provider adapter implementing the existing `provider.Provider`/`provider.Session` interface by supervising the Claude Code CLI.
- Named model roles (`implement`, `verify`) in `WORKFLOW.md`, each with its own provider and model.
- A verification step in the delivery pipeline, between successful validation and push.
- Per-provider account gates, usage records, and diagnostics, including a check of every configured model.

Out of scope: the Claude API/Agent SDK called directly from Go (no official Go Agent SDK exists; the CLI is the harness), automatic model escalation or downgrade, more than two roles, per-issue model overrides, cost-based routing, and the supervisor's advisory agent (SUPERVISOR_SPEC.md keeps it deferred; when built it SHOULD use a `plan` role on the same mechanism).

## 3. Configuration

### 3.1 Service configuration

New keys in the service configuration (`examples/config.yaml`):

```yaml
claude: claude                 # Claude Code executable
providers:                     # host-enforced allow-list; repository config cannot widen it
  codex:
    models: []                 # empty = any model the provider accepts
  claude:
    models: [claude-opus-5-5, claude-sonnet-5-5]
```

- A workflow that names a provider or model outside the allow-list is invalid configuration: dispatch for that project blocks with the reason in status (SPEC.md §5). This is the cost boundary — a repository cannot opt the host into a more expensive model.
- Omitting `providers` keeps today's behavior: Codex only, any model.
- `daemon start --claude <path>` mirrors `--codex`.

### 3.2 Workflow front matter

`WORKFLOW.md` gains an `agents` map keyed by role:

```yaml
---
agents:
  implement:
    provider: claude
    model: claude-opus-5-5
  verify:
    provider: claude
    model: claude-sonnet-5-5
    instructions: |
      Block on missing tests for changed behavior and on any change outside the issue's scope.
validation:
  - go test ./...
max_turns: 5
max_verify_rounds: 2
---
```

Rules:

- `implement` is required when `agents` is present. `provider` is required per role; `model` is required for `claude` and optional for `codex` (provider default).
- `verify` is optional. Its absence disables verification, which is the behavior of every existing workflow.
- Roles may use different providers (for example Codex implements, Claude Sonnet verifies). Each role opens its own session.
- `instructions` (verify only) is appended to the built-in verifier prompt. It is plain text, not a template.
- The legacy top-level `model` key remains valid and means `agents.implement = {provider: codex, model: <model>}`. Setting both `model` and `agents` is an error. Unknown keys under `agents` or a role are rejected, matching the existing strict decoder.
- `max_verify_rounds` (default 2, minimum 1) bounds how many times verification may fail before the attempt needs attention (§5.3).
- The resolved `agents` block is part of the workflow snapshot recorded on the attempt, so a mid-attempt edit to `WORKFLOW.md` affects only future attempts.

## 4. Claude provider adapter

### 4.1 Process and protocol

Each session is one supervised `claude` subprocess in print mode with streaming JSON on both stdin and stdout:

```text
claude -p --input-format stream-json --output-format stream-json --verbose \
  --model <model> [--resume <session-id>] <permission flags per role>
```

- One process per session, started in the attempt's worktree, in its own process group, with the same cancellation, `WaitDelay`, and SIGKILL fallback as the Codex adapter.
- The adapter owns line framing, bounded event buffering (overflow aborts the session, as Codex does, rather than dropping a turn-completion event), stderr capture into the attempt log, and graceful shutdown on stdin close.
- `Start` records the session ID from the CLI's init event; that ID is the attempt's `Thread`. `Start` with a non-empty `Thread` resumes it with `--resume`. An empty or missing session ID is a start failure.
- `Run` writes one user message and reads events until that turn's result event. A result with an error subtype, or process exit before a result, fails the turn with the CLI's reported reason.
- Unattended operation: interactive permission prompts or tool approval requests MUST never block. Any request the adapter cannot answer is denied with an explanatory message, as the Codex adapter does for client requests.
- The adapter MUST pin a tested Claude Code version. `doctor` reports a version mismatch as a warning; contract tests run against recorded stream-json fixtures for that version.

### 4.2 Permissions by role

| Role | Permission posture |
| --- | --- |
| `implement` | Full workspace access without prompts (equivalent of Codex `danger-full-access` + `approvalPolicy: never`), e.g. `--permission-mode bypassPermissions`. |
| `verify` | No file-editing tools; shell limited to read-only inspection and the workflow's validation commands; no network fetch tools. Project-level settings and hooks from the issue branch SHOULD NOT be loaded (e.g. restrict setting sources to user). |

Tool restriction is defense in depth, not the guarantee. The controller enforces verifier read-only behavior itself (§5.2).

### 4.3 Environment and credentials

- The subprocess environment is built from `core.PrivateEnvironment()` plus only what Claude Code needs to find its own credentials (home directory, config directory).
- `ANTHROPIC_API_KEY` and other API-billing credentials MUST NOT be passed unless the operator sets them explicitly in the service environment. A subscription account that hits its limit MUST pause (SPEC.md acceptance criterion 8), never fall through to pay-per-token billing.
- Variables that make Claude Code believe it is nested inside another Claude Code session MUST be stripped, so a daemon started from a Claude terminal behaves the same as one started by launchd/systemd.

### 4.4 Event normalization

Map stream-json events onto the existing normalized `Event`/`core.Output`:

| Claude event | Normalized |
| --- | --- |
| init | session started (`Thread` = session ID, `PID`) |
| assistant text | message delta |
| assistant tool use / tool result | tool activity (command, file path, exit status where present) |
| result (success) | turn completed, plus usage update (input, output, cache tokens, reported cost) |
| result (error) | turn failed |
| rate-limit / usage-limit signal | account limitation |

Every event carries the role and model that produced it so transcripts and `agent logs` can label Opus and Sonnet turns separately. Raw provider payloads remain available under `--raw`.

### 4.5 Steering

Removed. Changathi has no channel for messaging a running agent; the operator reviews on GitHub and requests rework there (SPEC.md §7, review loop). Rework feedback reaches the next `implement` turn's prompt, never a `verify` session.

### 4.6 Account limits

- Account gates become per provider: the gate record key changes from `codex` to the provider name, and dispatch checks the gate of the provider for the role about to run. A Claude limit does not pause Codex work, and the reverse.
- If the CLI reports a limit specific to one model (for example an Opus allowance exhausted while Sonnet is available), the gate is keyed by provider and model. An attempt waiting on the Opus gate stays in `waiting_account`; it is not continued on Sonnet.
- The adapter classifies limits from structured result fields first and falls back to the existing text match ("rate limit", "usage limit", "quota").

## 5. Verification step

### 5.1 Placement

```text
running(implement) -> validating -> verifying -> pr_open -> waiting_checks -> merging -> ...
```

`verifying` is a new execution state. It runs only after the controller's validation succeeds for the current head SHA (clean worktree, new commits, no closing keywords, validation commands passed). Verification never runs on a head that failed validation.

Verification binds to a head SHA. A verdict is reused, not recomputed, when delivery revalidates because only the base moved (SPEC.md §7 implementation note) and the merged result introduces no conflicts; any new agent commit requires a new verification.

### 5.2 Verifier session

- A fresh session with the `verify` role's provider and model. It never resumes the `implement` thread and never sees its transcript; independence from the author is the point of the step.
- Prompt inputs: issue number, title, and body; the diff from the merge base to the validated head; the validation commands and their results; the workflow's `verify.instructions`; and the required verdict format.
- Before the session the controller records `HEAD` and `git status --porcelain`. After it, both MUST be unchanged. Any change fails the attempt into `needs_attention` with "verifier modified the worktree", preserving the worktree for inspection. The controller does not attempt to revert it.
- The verifier session runs under the attempt's existing worker slot and timeout; it does not take an extra concurrency slot, because the `implement` session is closed first.

### 5.3 Verdict

The verifier MUST end with exactly one verdict, using the CLI's structured-output mode where the pinned version supports it, otherwise a single fenced block with language `changathi-verdict`:

```json
{
  "verdict": "pass",
  "findings": [
    {"severity": "blocking", "file": "internal/x.go", "line": 42, "summary": "..."}
  ]
}
```

- `verdict` is `pass` or `fail`. `severity` is `blocking` or `note`. A `fail` verdict MUST contain at least one `blocking` finding; a `pass` verdict MUST contain none. Anything else is malformed.
- Parse strictly; reject unknown fields. A missing, duplicated, or malformed verdict is never a pass: the verifier is re-run once on the same head, and a second malformed result moves the attempt to `needs_attention`.
- `pass`: proceed to push and PR. `note` findings are appended to the PR body under a "Verification notes" heading, and are also posted as a PR review comment (a `COMMENT` review, which GitHub permits on the author's own PR). The comment is journaled as an operation keyed by the verified head, so recovery never posts it twice; a later verified head posts its own comment.
- `fail`: the blocking findings become the prompt of the next `implement` turn on the existing thread. That turn counts against `max_turns`. After `max_verify_rounds` failing verdicts in one execution generation, the attempt moves to `needs_attention` with the latest findings as its reason.
- The verdict, findings, model, and head SHA are stored on the attempt and emitted as a `verification` event.

## 6. Data model and CLI changes

- Attempt: `provider` and `model` become per role. Store `agents` (resolved role → provider/model), the `implement` thread reference, and a list of verification records (head SHA, model, session ID, verdict, findings, usage, timestamps). Existing attempts without `agents` read as `implement = {codex, <workflow model>}`.
- Usage: aggregate per attempt, per role, per provider and model. `changathi usage` reports Codex account limits as today and, for Claude, the locally aggregated token and cost totals per model since the last limit reset it observed, with any limit state from §4.6.
- `agent show` lists each role's provider/model and the latest verdict; `agent list` adds a compact `verify: pass|fail|-` column. `agent logs` labels turns `[implement opus]` / `[verify sonnet]`.
- `summary` reports provider gates per provider (and per model where keyed so).
- A retry that changes the `implement` provider (for example a workflow switched from Codex to Claude) starts a new session; a thread is never resumed across providers. Changing only the Claude model within the provider may resume the existing session with the new model.

## 7. Diagnostics: checking every model

`changathi doctor` gains, for each provider referenced by any enabled project's workflow:

1. The executable is on the path, and its version matches the pinned version (mismatch is a warning).
2. The CLI reports an authenticated account.
3. Each configured model, across all roles of all enabled projects, is on the host allow-list (§3.1).

`changathi doctor --models` additionally runs one minimal non-tool turn per distinct (provider, model) pair, in a temporary empty directory, and reports per model: reachable, rejected (unknown or unentitled model), or limited. This spends a small amount of quota, so it is opt-in and never part of daemon startup. `project add` and workflow reload run checks 1–3 for that project and surface failures in status.

## 8. Recovery

- Attempts left in `verifying` at startup move to `retry_wait` under the existing per-attempt lock rule; on resume, the controller re-checks the worktree against the recorded head and reruns verification from scratch. A verifier session is never resumed.
- An `implement` session is resumed from the stored Claude session ID. If the CLI reports the session as missing (for example local session history was cleared), the adapter starts a new session and the first prompt includes the issue, the current diff, and the last failure reason; the event log records the lost session.

## 9. Acceptance criteria

1. A project with `agents.implement = claude/claude-opus-5-5` completes an issue end to end (implement, validate, PR, checks, merge, `awaiting_review`) with no Codex process started.
2. Every implementation turn and every verification turn is recorded with the model that ran it; verification turns always run on `claude-sonnet-5-5` in the default configuration.
3. A workflow naming a model outside the host allow-list blocks dispatch for that project with a clear reason, while other projects continue.
4. A failing verdict produces a new Opus turn with the findings in its prompt; after `max_verify_rounds` failures the attempt is `needs_attention`, and the PR was never opened for an unverified head.
5. A malformed or missing verdict is never treated as a pass.
6. A verifier that edits or commits in the worktree is detected and the attempt stops with the worktree preserved.
7. A Claude account limit pauses only work whose next role uses Claude, preserves the worktree and session, and never falls back to API-key billing or to a different model.
8. (Removed with agent messaging.)
9. Daemon restart during `implement` resumes the same Claude session; restart during `verifying` reruns verification.
10. Existing Codex-only workflows (`model` key, no `agents`) behave exactly as before, with no verification step.
11. Provider contract tests replay recorded stream-json fixtures through a fake process; integration tests exercise the pinned Claude Code version on macOS and Linux.
12. `doctor --models` reports each configured model's reachability independently.

## 10. Implementation sequence

1. Per-provider gates and usage records; `provider` selection in the engine (no behavior change for Codex).
2. Claude adapter: process supervision, stream-json framing, session start/resume, event normalization, fixtures and contract tests.
3. `agents` workflow schema, host allow-list, legacy `model` mapping, `doctor` checks.
4. `verifying` state, verifier session, worktree guard, verdict parsing, fix-up loop, PR notes.
5. CLI output (`agent show/list/logs`, `summary`, `usage`), skill update, `doctor --models`.

## 11. Open questions

- Whether the verifier should also run after a check-failure fix-up turn whose diff is small, or only after turns that touched non-test code. V1 verifies every new head.

## 12. Implementation notes

- **Adapter.** `provider.Claude` launches the CLI in `Start` (not `Open`) because the session ID and permission posture are per request. New sessions pass a generated UUID with `--session-id`, so the thread ID is known before the first event; resumes pass `--resume`. Each `Run` writes one stream-json user message and returns at that turn's `result` event. Implementers run with `--permission-mode bypassPermissions --permission-prompts none`; verifiers with `--permission-mode dontAsk --setting-sources user --no-session-persistence --tools Read,Grep,Glob,Bash` and an allow-list of `Read`, `Grep`, `Glob` and `git diff/log/show/status`. The verifier prompt names that surface, including that other commands, pipes and chained commands are denied and that validation has already run, so the verifier doesn't spend calls on denied `cat` or `go test` commands. `--effort` is passed when a role sets `effort`, which is accepted for the `claude` provider only. Verdicts use `--json-schema` and are read from the result's `structured_output`; the fenced block remains the fallback for providers without structured output.
- **Codex verifier.** A Codex `verify` role starts its thread with `sandbox: read-only`; it has no structured-output mode, so its verdict comes from the fenced block.
- **Account limits.** A `rate_limit_event` whose `rate_limit_info.status` is `rejected`, or an assistant `error` of `rate_limit` or `billing_error`, becomes a `Claude usage limit (<rateLimitType>)` error. A gate is model-specific when the limit text names the role's model family (for example `seven_day_opus` for `claude-opus-5-5`); otherwise it covers the whole provider. The account gate record key is `<provider>` or `<provider>/<model>`; legacy `codex` gates keep working.
- **Usage.** Changathi aggregates turns, tokens, and reported cost per Claude model across all attempts, plus the last observed rate-limit window, and `changathi usage` shows them next to Codex's live account limits. Per-attempt, per-role usage is not recorded.
- **Transcripts.** A `role` event (`implement claude claude-opus-5-5`, `verify claude claude-sonnet-5-5`) precedes each session's events, and a `verification` event records each verdict with its findings; individual lines are not prefixed with the role.
- **Verification counting.** `VerifyFailures` on the attempt counts failing verdicts and resets on an explicit `issue retry`, like `Turns`; the full verification history is kept.
- **Diagnostics.** `doctor` checks the providers enabled in the host configuration (Codex only when `providers` is absent) rather than scanning project workflows; workflow roles are checked against the allow-list when the daemon loads each workflow, and a violation is shown as the project's error. `doctor --models` probes each model on the Claude allow-list. The CLI version is compared with `provider.ClaudeTestedVersion` and a mismatch is a warning.
- **Lost sessions.** Claude Code saves a session once it has received the first message; the session survives an API error or a kill mid-turn. Resuming a session that was never saved, or whose history was deleted, fails with `No conversation found with session ID` on stderr, which the adapter reports as `provider.ErrSessionLost`. The engine then records a `session_lost` event, opens a fresh session, and reruns the same turn (not counted twice) with a recovery prompt: the issue, the branch's commits, changed files and uncommitted changes, the attempt's `LastFailure`, and the turn's own instruction. Codex threads that fail to resume are not detected and still fail through the retry path.
- **Resuming without an implement turn.** Once an implement turn completes, the attempt records `NextStep: validate`. Re-entry after a restart, an interrupted validation or verification, or a wait on the verifier's account gate skips the implement turn (and never opens the implement session): it revalidates, reuses a recorded `pass` for the same head instead of re-verifying, and publishes. `NextStep` clears when validation, verification or a CI check fails, and on rework (`issue retry` or `agent:rework`).
- **Failure context.** `LastFailure` holds only failures the agent can act on: validation output, blocking findings, and failing checks. Operational state changes (restarts, account waits, interruptions) update `Error` for the operator but never `LastFailure`, and the implement prompt includes `LastFailure` instead of `Error`. It clears when a head passes validation and verification.
- **Rework context.** A new generation records `Previous` (generation, PR, final state, and last failure, or the "closed without merging" reason). Its first prompt says whether that PR was merged, so the work is already on the branch, or not merged. Rework also adds the operator's issue comments captured when it was requested, with the `changathi-reply` instructions for answering without a code change (SPEC.md §7).
