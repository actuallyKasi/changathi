# Changathi supervisor implementation specification

Status: deterministic supervisor implemented, 2026-10-08; plans from GitHub sub-issues and closure-only approval, 2026-10-19; optional advisory agent deferred. This document extends [SPEC.md](SPEC.md). MUST denotes a requirement; SHOULD denotes a recommendation. CLI commands and configuration below describe the implemented interface; implementation notes qualify the original design.

## 1. Purpose

Coordinate a set of dependent GitHub issues through a parent issue labelled `supervisor`. Changathi selects eligible children, runs them through its existing delivery pipeline, and waits for human completion of prerequisites before advancing. The parent states the goal; the graph is GitHub's own issue relationships: the parent's sub-issues are the children and each child's blocked-by links are its dependencies.

The initial target is the homelab sequence: configuration decisions (#3), node preparation (#4), kubeadm/Cilium bootstrap (#5), cluster validation (#6), Argo CD (#7), ARC/CI (#8), and deployment/rollback validation (#9).

The supervisor is a durable daemon controller. Dependency checking and dispatch MUST work without an LLM session or an occupied worker slot. An optional advisory agent is a later extension for planning and evaluating blockers. This avoids deadlock when worker concurrency is one.

## 2. Scope and defaults

V1 supports one repository per plan, existing child issues, and a directed acyclic dependency graph. Every child whose prerequisites are satisfied may be queued at once; global/project implementation concurrency alone limits how many run, so a single-session provider account is expressed as global `concurrency: 1`, not as plan serialization. Independent work in other plans or projects may proceed while a plan waits for review.

V1 excludes cross-repository dependencies, nested supervisors, automatic issue decomposition, autonomous prerequisite skipping, and automatic infrastructure authorization. Parent/child issue creation is an operator action. The supervisor never closes issues on the operator's behalf.

Parent tagging and execution are separate concepts: the `supervisor` label identifies a plan; automatic discovery also requires the project's existing `auto` setting. For `auto: false`, the operator explicitly starts the parent. Starting the parent authorizes scheduling its children under the recorded plan, without requiring an individual `issue run` for every child.

Starting a plan does not supply missing credentials or override restrictions on live infrastructure in its child issues. Those requirements must be resolved before the affected child becomes executable.

## 3. Parent issue contract

An open parent MUST carry `supervisor`. Its body explains the goal and is not parsed. The plan is derived from GitHub on every reconciliation:

- **Children** are the parent's sub-issues (`GET /repos/{repo}/issues/{parent}/sub_issues`), in GitHub's sub-issue order.
- **Dependencies** are each child's blocked-by issues (`GET /repos/{repo}/issues/{child}/dependencies/blocked_by`).
- **Policy** comes from child labels. `changathi:evidence` sets `requires_evidence` (§5). `changathi:manual` sets `completion: closed_completed`: a manual gate that queues no implementation worker and still requires human completion. Without it, `completion` is `merged_and_closed`. `project add` creates both labels.

Example for homelab: label the parent `supervisor`; add #3–#9 as its sub-issues; mark #4 blocked by #3, #5 by #4, and so on to #9 by #8; label #4–#9 `changathi:evidence`. See [examples/supervisor-issue.md](examples/supervisor-issue.md).

Validation rejects: no sub-issues, sub-issues from another repository, pull requests, children labelled `supervisor`, the parent as a child, duplicates, cycles, more than 100 children or 500 edges, and any blocker that is not itself a sub-issue of the same parent. The last rule keeps the graph closed: a plan never silently waits on, or ignores, an issue outside it. Dependencies are sorted, so blocker order on GitHub does not change the plan. Resolve immutable issue IDs before enrollment; surface errors without partial scheduling.

Sub-issue order breaks ties between eligible children. A child without blockers is eligible immediately, subject to all other gates. Never infer an edge from prose or numeric issue order.

**Deprecated fenced plan.** A parent whose body contains a ```` ```changathi-supervisor ```` block keeps using it, so plans enrolled before 2026-10-19 work unchanged. The block is strict versioned YAML (`version: 1`, `children` with `issue`, `depends_on`, `completion`, `requires_evidence`); unknown fields, a second block or document, and the same graph errors as above are rejected. The plan's source is part of its revision, so moving a plan from the body to sub-issues is an ordinary graph change (§4): remove the block and the next pass adopts the sub-issues, subject to the same safety rules. The workpad marks a fenced plan as deprecated.

## 4. Activation and plan revisions

- In an enabled project with `auto: true`, discover open `supervisor` issues through paginated polling. A valid new plan is enrolled and activated. The parent does not need `agent:ready`.
- In a project with `auto: false`, `supervisor start <project>#<parent>` validates, enrolls, and activates it explicitly. The `supervisor` label is still required. `start` also restarts a cancelled or finished plan.
- A parent MUST never enter the ordinary implementation queue, even if it also carries `agent:ready`. Ordinary `issue run` on a parent returns an instruction to use `supervisor start`.
- Snapshot the canonical graph and its hash (the revision) with its source and activation origin. Each dispatch records the revision that authorized it, for audit.
- The graph is re-read on every pass; there is no reload command. A changed revision is adopted automatically, in one enrollment transaction, unless it would orphan or re-gate work in flight. These changes are refused: removing a dispatched child, changing a dispatched child's completion policy, adding a blocker to a dispatched child that is not done, and adding a child owned by another plan or with a conflicting attempt. A refused or invalid graph holds the plan in `needs_attention` with a `plan change rejected:` reason and stops its workers at the next boundary. The hold is re-evaluated every pass, so undoing the edit on GitHub releases it without an operator command. Removing a blocker, adding an undispatched child, or reordering takes effect at once.
- A failure to read the graph (API outage, rate limit) is a remote wait (`waiting_remote`, §8), never a plan hold.
- Adopting a revision releases ownership only of removed children that were never dispatched. A running child keeps its attempt; the gate checks it against the current graph, not the revision it was dispatched under.
- Pausing/removing the label/closing the parent does not erase its enrollment or ownership records. Re-adding a label must not undo an explicit operator pause/cancel. Resume requires an explicit command.

Plan enrollment is serialized with normal enqueue operations. Do not adopt a child that already has a conflicting active or awaiting-review attempt: report the conflict and require operator reconciliation. Previously completed external work may be accepted under the evidence rules below.

## 5. Completion and human-review gates

A dependency is satisfied only when its latest observed GitHub state is `closed` with reason `completed`, and its completion policy is satisfied. `not_planned`, a missing/unknown reason, an inaccessible issue, or an ambiguous closure never counts as success.

For `merged_and_closed`, also require a merged PR targeting the registered repository's configured base branch. For managed delivery, use the recorded attempt/generation and PR, with successful existing validation/check gates. Never choose an arbitrary PR from issue prose. For pre-existing external work, require the operator to associate a PR explicitly using `supervisor reconcile --child ... --pr ...`; verify its repository, base, merge state, and record its evidence provenance.

A merged PR with an open issue remains `awaiting_review`. A completed agent turn, successful CI, `agent:review`, or a parent checkbox does not satisfy the dependency by itself.

A qualifying closure must be attributable to the configured human operator, after delivery for code issues. **That closure is the approval**: there is no separate approval comment, and nothing about its timing relative to other comments matters. The GitHub adapter retrieves closure/reopen events and actor identity; a bot closure or one performed by a commit or app is ambiguous. Only then is `supervisor reconcile --child ... --ack-completion` needed, as an explicit operator attestation bound to that closure and delivery head. Acknowledgement cannot bypass a missing merge, `not_planned` closure, or an open issue. Record source events and operator acknowledgements durably.

Human completion attests that the child's stated acceptance criteria are satisfied. For infrastructure work, this includes required live application and verification, not merely merged playbooks. Mark such children `requires_evidence` (label `changathi:evidence`). Their evidence is a property of the delivery, checked by Changathi rather than approved separately:

1. The merged PR adds or changes exactly one Markdown report under `docs/validation/`: the evidence is that file at the PR's head commit. Several reports link the PR's file list.
2. Otherwise, a line `Evidence: https://…` in the PR body.
3. Otherwise, an HTTPS URL recorded with `supervisor reconcile --child ... --evidence URL`. It is bound to the current delivery head; a later generation needs its own evidence.

The implementing agent of a `changathi:evidence` child is told to commit the report. While the child awaits review, the workpad shows the evidence link or that it is missing, so the operator can request rework before closing. A child closed without evidence does not satisfy the dependency and holds the plan; rework it, or record evidence with `reconcile --evidence`, which re-checks and releases the hold immediately. Changathi does not interpret the evidence document; the human does, before closing. Evidence URLs must be HTTPS with a host and no credentials.

Re-fetch prerequisites before dispatch and before delivery/merge. If a prerequisite reopens or its evidence is invalidated, stop new dependent turns/delivery, preserve work, and enter `needs_attention`. Do not roll back already merged code or live infrastructure. GitHub events and merges are not one atomic transaction: if invalidation races a completed merge, surface it for reconciliation and block further advancement.

## 6. Scheduling and concurrency

Each reconciliation pass:

1. Check project enablement, parent state/label, activation, plan revision, and cancellation state.
2. Refresh child issue/PR/review evidence. Unknown remote state blocks dependent dispatch; retain the previous snapshot for display only.
3. Reconcile any owned attempt with the existing delivery state machine.
4. Classify every unfinished child: in progress, awaiting human review or evidence, needing attention, or ready (all prerequisites satisfied and no owned attempt). Report them all with concrete reasons.
5. Select every ready child, in plan order. Completed children are skipped; a closed-but-unsatisfied child (including one closed without required evidence) holds the whole plan.
6. Atomically claim/enqueue each with trigger `supervisor`, plan ID/revision, child issue ID, and execution generation. The existing scheduler applies project/global capacity and fairness.
7. Publish progress only when the persisted state changes.

The supervisor does not add `agent:ready` to children. Owned children must be excluded from ordinary label-based auto pickup. Manual `issue run/retry` and the `agent:rework` label for an enrolled child route through the same dependency gate and ownership checks; they cannot bypass prerequisites. Rework of a delivered (`awaiting_review`) child starts a new generation; its dependants keep waiting, because completion requires closure after the latest generation's merged delivery. Review a child before closing it: closing and then reopening a prerequisite puts the plan into `needs_attention`. Reject execution while the parent is paused/cancelled, with instructions for operator reconciliation.

One child may belong to only one enrolled non-archived plan. Ownership and attempt creation must be transactional, with a uniqueness invariant on repository ID plus child issue ID. Repeated polls, duplicate API requests, or concurrent manual/automatic triggers must produce one generation, not competing workers.

Supervision consumes no worker slot while polling, waiting for review, or waiting for GitHub/account recovery. Existing worker concurrency controls govern runnable implementation work. Supervisors use bounded API work and round-robin project scheduling so many waiting plans cannot starve normal jobs.

## 7. State, pause, and completion

Supervisor states: `queued`, `active`, `waiting_capacity`, `waiting_child`, `waiting_review`, `waiting_remote`, `paused`, `needs_attention`, `awaiting_parent_review`, `done`, `cancelled`.

Every waiting/attention state stores a reason and wake condition. After all children satisfy their policies, publish a summary and enter `awaiting_parent_review`. The parent remains open. Human closure as completed transitions it to `done`. Premature parent closure stops work and requires reconciliation; closure as not planned cancels the plan. Reopening a completed parent does not rerun children automatically.

`pause` prevents new turns and delivery. Interrupt an active child through the existing cancellation/stop mechanism at the next available boundary, preserving its branch, worktree, and provider session where supported. An operation already confirmed remotely is reconciled, never blindly reversed. `resume` revalidates dependencies and resumes the interrupted generation rather than creating duplicate work.

`cancel` stops scheduling and owned active execution, preserving results. Resuming a cancelled plan requires an explicit retry/reconciliation action. Project pause has equivalent execution restrictions. Removing the parent label pauses it and prevents subsequent delivery. Pausing/cancelling a parent never closes child issues or discards their work.

A child failure exhausts existing bounded retries before marking that child as needing attention; the plan reports `needs_attention` but is not held, so independent branches keep running and only the failed child's descendants wait. `issue retry` or the `agent:rework` label re-queues the failed child. Never skip, close, or relabel a failed prerequisite as completed to continue.

## 8. Persistence and restart recovery

Extend the existing SQLite store with:

- Supervisor record: parent immutable identity, project, activation origin, state/reason, plan revision/hash, pause/cancel latch, current child, and workpad comment ID.
- Plan revision and child records: issue identity, dependency edges, completion policy, ownership, accepted evidence, and required operational attestations.
- Attempt linkage: supervisor ID, revision, child identity, generation, and dispatch idempotency key.
- Operation/event records: intent/results for enqueue, workpad publication, operator acknowledgements, pause/resume, and evidence invalidation.

Use versioned migration and transactions within the existing store architecture; independent `Put` calls are insufficient for atomic enrollment/claiming. Existing projects and attempts remain valid with supervision disabled.

On startup, reconcile remote issue/PR state, owned attempts, and surviving worker locks before dispatching. Do not replay an acknowledged dispatch, restart awaiting-review work, or lose explicit pauses. Persist state before remote status updates. Workpad errors cannot create a second attempt. On API failure or rate limiting, back off using existing policy, show stale/unknown observations, and retry reconciliation without spending worker slots.

## 9. GitHub and CLI experience

Maintain one parent comment headed `Changathi supervisor`, using a hidden stable plan identifier. Show plan revision and source, current state, next eligible child or blocker, and a table of child dependencies, delivery PR, review status, and evidence links. Recover the comment by marker after ambiguous creation responses. Retry updates idempotently and edit only Changathi's own comment. Avoid repetitive comments for unchanged polls.

Commands:

```text
changathi supervisor validate homelab#PARENT
changathi supervisor start homelab#PARENT
changathi supervisor list --project homelab
changathi supervisor show homelab#PARENT
changathi supervisor pause|resume|cancel homelab#PARENT
changathi supervisor reconcile homelab#PARENT --child 4 --pr PR_NUMBER
changathi supervisor reconcile homelab#PARENT --child 4 --evidence URL
changathi supervisor reconcile homelab#PARENT --child 4 --ack-completion
```

`validate` is read-only and reports graph, ownership, and eligibility problems for the plan as GitHub currently defines it. `reconcile` options may be combined; a reconciliation that resolves an attention hold re-checks the plan at once, without `resume`. All mutations use request IDs and the local daemon API. Structured `--json` responses include blocked child IDs and precise reasons. No generic force/skip gate is included in v1.

`summary` and `supervisor list` show supervisor state separately from implementation agents, e.g. `homelab#PARENT: waiting_review on #4; #5 blocked by #4`. Supervision must not appear as one active worker or conceal available capacity.

## 10. Implementation map

- `internal/core/model.go`: supervisor/revision/child/evidence models, attempt linkage, issue state reason and closure event fields.
- `internal/core/github.go`, `supervisor_github.go`: labelled-parent discovery, sub-issue and blocked-by reads, paginated review events, PR evidence lookup, idempotent workpad operations.
- `internal/core/store.go`: migrations, atomic ownership/enqueue operations, persisted supervisor transitions.
- New `internal/core/supervisor.go`: native plan construction, deprecated fenced-plan parsing, graph validation.
- New `internal/service/supervisor.go`: reconciliation and child handoff into the existing scheduler.
- `internal/service/engine.go`: shared dependency/parent gates in enqueue, turns, and delivery; exclude parents/owned children from normal automatic pickup. Its current `awaiting_review` logic accepts any closed issue and must not be reused as supervisor completion proof.
- `internal/service/server.go`, `internal/cli/cli.go`, and `internal/cli/client.go`: API, commands, and operator reconciliation.

Repository `WORKFLOW.md` continues to configure implementation agents and validation. Parent issue configuration cannot raise host concurrency, weaken sandbox/credential boundaries, or override merge policy. Existing delivery code remains responsible for PR validation and merging.

## 11. Optional supervisor agent

After deterministic supervision is reliable, add bounded advisory turns for proposing graphs, summarizing failures, or assessing missing evidence. Such turns use a separately configured supervisor-agent limit (default disabled), a shared provider/account budget, timeouts, and usage accounting. They release their slot when waiting and never reserve a worker slot while requesting child execution.

The advisory agent submits proposals to the daemon; it cannot bypass dependencies, declare human review complete, close issues, merge PRs, or alter authorization. Graph changes still follow the adoption rules in §4. Scheduling and restart recovery must remain correct with this feature disabled or its provider unavailable.

## 12. Acceptance tests and rollout

Use fake GitHub/provider adapters and a controllable clock. Required tests:

1. At global/project worker concurrency one, parent starts child A, waits after A's PR merges, then starts B only after A's qualifying human completion. No idle supervisor occupies a slot.
2. `auto: false` does not start a labelled parent; explicit start advances eligible children. `auto: true` discovers a labelled parent once.
3. Ordinary auto/manual dispatch cannot run a parent as a worker or bypass an enrolled child's dependencies. Racing triggers create one generation.
4. Invalid graphs, foreign/missing issues, blockers outside the plan, cycles, duplicated ownership, and conflicting existing attempts fail before any dispatch. Unsafe graph edits hold the plan until undone; safe edits are adopted without operator action.
5. Closed-as-not-planned, missing merge, unknown closure reason/actor, bot closure, and a delivery without required evidence all block advancement. A human closure of a delivery with evidence advances without any further action.
6. A completed external prerequisite is accepted only through explicit verified evidence association. Human reconciliation is auditable.
7. Parent edits, pause, label removal, cancellation, and project pause stop later turns/delivery; resume preserves unfinished work.
8. A reopened prerequisite before dispatch or during execution blocks its descendants; an already completed merge is surfaced without destructive rollback.
9. Restart after claim, PR publication, merge, workpad timeout, or issue closure preserves exactly one active generation and reconstructs the right wait state.
10. GitHub outages/rate limits never satisfy a dependency from stale data, occupy worker capacity, or cause repeated workpad comments.
11. Completing all children leaves the parent awaiting human review. Premature closure, reopening, and unchanged polling do not cause unrequested execution.
12. Multiple plans/projects remain fair, and unsupervised existing workflows retain their current behavior.

Implement in stages: schema/parser and persistence; read-only validation/status; explicit-start reconciliation and dispatch; lifecycle/recovery tests; automatic discovery and workpad; optional advisory agent afterward. Roll out first with a disposable repository and two dependent issues, then a homelab parent. Creating this specification does not start a supervisor or change the existing homelab issues.

## Implementation notes

The controller is deterministic; advisory agent support remains deferred. The authenticated `gh` user is the human operator. `requires_evidence` is an explicit child policy (a label) because operational requirements cannot reliably be inferred from issue text. `closed_completed` children are manual gates and never create code workers.

Each reconciliation reads the sub-issues and one blocked-by list per child, in addition to the per-child issue, delivery and closure reads; evidence lookups are cached per delivery head.

Supervisor records and canonical revisions use new kinds in the existing SQLite records store, with schema version 1 in the plan; enrollment and ownership changes share one transaction. Core workers continue through the existing delivery pipeline. Remote reconciliation failures back off up to four minutes; workpad failures retry after two minutes. Reconciled comments use stable markers and journaled publication intent. All remote evidence is checked again before dispatch/delivery; GitHub cannot provide an atomic transaction spanning closure state and merge.

Automated verification uses fake GitHub/provider adapters and local Git repositories. Tests cover actual worker delivery with concurrency one, human completion gates, explicit evidence, plan expansion, ownership races, SQLite reopen, interruption, API failure, comment recovery, and CLI requests. They do not perform real GitHub mutations or live infrastructure changes.


## Attempt supersession

Old awaiting-review attempts become superseded when replaced by a new generation. Startup repairs historical stale rows. Superseded attempts are inactive, retain audit history, and never consume a worker slot.

## Closure-only approval and native plans (2026-10-19)

Between 2026-10-09 and 2026-10-19, operational evidence was approved by a second human step: after closing the child, the operator posted an exact `/approve-evidence #PR URL` comment that had to be a new, unedited comment created after the closure. In practice that duplicated the closure (same person, same moment) and failed on ordering alone. It is removed: closure is the approval, and evidence is checked on the delivery itself (§5). Old approval comments are ignored, and rework still skips them when collecting operator feedback.

The fenced YAML plan with its `reload` / `reload --apply` / `resume` cycle is replaced by GitHub sub-issues and blocked-by links, re-read every pass (§3, §4). The fenced form remains as a deprecated fallback for parents that still contain it.

## Parallel children (2026-10-12)

The original one-unfinished-child rule existed because the operator's Codex plan allowed one concurrent session. That is now expressed as the global default `concurrency: 1`. The supervisor records every selected child in `ready` (with `current` kept as a display hint) and queues them all; the scheduler runs them as capacity allows. A failed or cancelled child no longer cancels running siblings. Plan-wide holds remain for graph changes, label removal, pauses, unsatisfied closures, and regressed prerequisites of dispatched children.
