# Changathi supervisor implementation specification

Status: deterministic supervisor implemented, 2026-10-08; optional advisory agent deferred. This document extends [SPEC.md](SPEC.md). MUST denotes a requirement; SHOULD denotes a recommendation. CLI commands and configuration below describe the implemented interface; implementation notes qualify the original design.

## 1. Purpose

Coordinate a set of dependent GitHub issues through a parent issue labelled `supervisor`. Changathi selects eligible children, runs them through its existing delivery pipeline, and waits for human completion of prerequisites before advancing. A parent contains the scope, ordered dependency graph, and completion requirements.

The initial target is the homelab sequence: configuration decisions (#3), node preparation (#4), kubeadm/Cilium bootstrap (#5), cluster validation (#6), Argo CD (#7), ARC/CI (#8), and deployment/rollback validation (#9).

The supervisor is a durable daemon controller. Dependency checking and dispatch MUST work without an LLM session or an occupied worker slot. An optional advisory agent is a later extension for planning and evaluating blockers. This avoids deadlock when worker concurrency is one.

## 2. Scope and defaults

V1 supports one repository per plan, existing child issues, a directed acyclic dependency graph, and at most one unfinished child execution per plan. Global/project implementation concurrency remains authoritative. Independent work in other plans or projects may proceed while a plan waits for review.

V1 excludes cross-repository dependencies, nested supervisors, automatic issue decomposition, autonomous prerequisite skipping, and automatic infrastructure authorization. Parent/child issue creation is an operator action. The supervisor never closes issues on the operator's behalf.

Parent tagging and execution are separate concepts: the `supervisor` label identifies a plan; automatic discovery also requires the project's existing `auto` setting. For `auto: false`, the operator explicitly starts the parent. Starting the parent authorizes scheduling its children under the recorded plan, without requiring an individual `issue run` for every child.

Starting a plan does not supply missing credentials or override restrictions on live infrastructure in its child issues. Those requirements must be resolved before the affected child becomes executable.

## 3. Parent issue contract

An open parent MUST carry `supervisor` and exactly one fenced block whose language is `changathi-supervisor`. Narrative outside the block explains the goal; it is not parsed for dispatch decisions. Checkboxes and free-form dependency comments are display information only.

Example parent body for homelab (the parent itself is a separate issue):

````markdown
Bring the homelab from its verified VM baseline to a tested GitOps delivery path.

```changathi-supervisor
version: 1
children:
  - issue: 3
    depends_on: []
    completion: merged_and_closed
  - issue: 4
    depends_on: [3]
    completion: merged_and_closed
  - issue: 5
    depends_on: [4]
    completion: merged_and_closed
  - issue: 6
    depends_on: [5]
    completion: merged_and_closed
  - issue: 7
    depends_on: [6]
    completion: merged_and_closed
  - issue: 8
    depends_on: [7]
    completion: merged_and_closed
  - issue: 9
    depends_on: [8]
    completion: merged_and_closed
```
````

All references are issue numbers in the registered repository. `completion` defaults to `merged_and_closed`. `closed_completed` is a manual completion gate for work that needs no code delivery; it queues no implementation worker and still requires human completion. A documentation/report PR may use the default.

Parse with a strict versioned schema. Reject unknown fields, empty graphs, duplicate children, missing dependencies, cycles, self-reference, the parent as a child, PR numbers, and children labelled `supervisor`. Resolve repository/issue immutable IDs before enrollment. Proposed limits: 100 children and 500 edges per plan; surface limit errors without partial scheduling.

List order breaks ties between eligible children. An empty dependency list permits immediate eligibility, subject to all other gates. Never infer an edge from prose or numeric issue order.

## 4. Activation and plan revisions

- In an enabled project with `auto: true`, discover open `supervisor` issues through paginated polling. A valid new plan is enrolled and activated. The parent does not need `agent:ready`.
- In a project with `auto: false`, `supervisor start <project>#<parent>` validates, enrolls, and activates it explicitly. The `supervisor` label is still required.
- A parent MUST never enter the ordinary implementation queue, even if it also carries `agent:ready`. Ordinary `issue run` on a parent returns an instruction to use `supervisor start`.
- Snapshot the canonical graph, its hash, the source body/update time, and activation origin. Each dispatch records the plan revision that authorized it.
- A changed parent body requires validation. If only narrative changed and the canonical graph is identical, update the display snapshot. A graph change puts the plan into `needs_attention`; preview it with `supervisor reload`, then adopt it with `supervisor reload --apply`. Reload leaves the plan paused until resume.
- Reload cannot remove or transfer a child with active execution, invalidate completed prerequisites silently, or change completion policy for already accepted work without explicit reconciliation. Refuse unsafe edits with a precise reason.
- Pausing/removing the label/closing the parent does not erase its enrollment or ownership records. Re-adding a label must not undo an explicit operator pause/cancel. Resume requires an explicit command.

Plan enrollment is serialized with normal enqueue operations. Do not adopt a child that already has a conflicting active or awaiting-review attempt: report the conflict and require operator reconciliation. Previously completed external work may be accepted under the evidence rules below.

## 5. Completion and human-review gates

A dependency is satisfied only when its latest observed GitHub state is `closed` with reason `completed`, and its completion policy is satisfied. `not_planned`, a missing/unknown reason, an inaccessible issue, or an ambiguous closure never counts as success.

For `merged_and_closed`, also require a merged PR targeting the registered repository's configured base branch. For managed delivery, use the recorded attempt/generation and PR, with successful existing validation/check gates. Never choose an arbitrary PR from issue prose. For pre-existing external work, require the operator to associate a PR explicitly using `supervisor reconcile --child ... --pr ...`; verify its repository, base, merge state, and record its evidence provenance.

A merged PR with an open issue remains `awaiting_review`. A completed agent turn, successful CI, `agent:review`, or a parent checkbox does not satisfy the dependency by itself.

A qualifying closure must be attributable to the configured human operator, after delivery for code issues. Extend the GitHub adapter to retrieve closure/reopen events and actor identity; a bot or an ambiguous automatic PR-associated closure requires attention. If human intent cannot be established from API data, require an explicit operator acknowledgement through `supervisor reconcile --child ... --ack-completion`. Acknowledgement cannot bypass a missing merge, `not_planned` closure, or an open issue. Record source events and operator acknowledgements durably.

Human completion attests that the child's stated acceptance criteria are satisfied. For infrastructure work, this includes required live application and verification, not merely merged playbooks. The workpad MUST distinguish code delivery from operational validation. If required live checks are reported unrun, hold the plan for attention even if the issue is closed; operator reconciliation must reference subsequent evidence satisfying them. Set `requires_evidence: true` explicitly for such children. V1 requires an operator acknowledgement with an HTTPS evidence URL for these outcomes; it does not infer operational requirements from prose or interpret the evidence document.

Re-fetch prerequisites before dispatch and before delivery/merge. If a prerequisite reopens or its evidence is invalidated, stop new dependent turns/delivery, preserve work, and enter `needs_attention`. Do not roll back already merged code or live infrastructure. GitHub events and merges are not one atomic transaction: if invalidation races a completed merge, surface it for reconciliation and block further advancement.

## 6. Scheduling and concurrency

Each reconciliation pass:

1. Check project enablement, parent state/label, activation, plan revision, and cancellation state.
2. Refresh child issue/PR/review evidence. Unknown remote state blocks dependent dispatch; retain the previous snapshot for display only.
3. Reconcile any owned attempt with the existing delivery state machine.
4. If this plan has an unfinished child (including awaiting human review, retry, CI wait, or attention), wait with a concrete reason.
5. Select the first open child in plan order whose prerequisites satisfy their gates. Completed children are skipped; closed-but-unsatisfied children block the plan.
6. Atomically claim/enqueue it with trigger `supervisor`, plan ID/revision, child issue ID, and execution generation. The existing scheduler applies project/global capacity and fairness.
7. Publish progress only when the persisted state changes.

The supervisor does not add `agent:ready` to children. Owned children must be excluded from ordinary label-based auto pickup. Manual `issue run/retry` for an enrolled child routes through the same dependency gate and ownership checks; it cannot bypass prerequisites. Reject execution while the parent is paused/cancelled, with instructions for operator reconciliation.

One child may belong to only one enrolled non-archived plan. Ownership and attempt creation must be transactional, with a uniqueness invariant on repository ID plus child issue ID. Repeated polls, duplicate API requests, or concurrent manual/automatic triggers must produce one generation, not competing workers.

Supervision consumes no worker slot while polling, waiting for review, or waiting for GitHub/account recovery. Existing worker concurrency controls govern runnable implementation work. Supervisors use bounded API work and round-robin project scheduling so many waiting plans cannot starve normal jobs.

## 7. State, pause, and completion

Supervisor states: `queued`, `active`, `waiting_capacity`, `waiting_child`, `waiting_review`, `waiting_remote`, `paused`, `needs_attention`, `awaiting_parent_review`, `done`, `cancelled`.

Every waiting/attention state stores a reason and wake condition. After all children satisfy their policies, publish a summary and enter `awaiting_parent_review`. The parent remains open. Human closure as completed transitions it to `done`. Premature parent closure stops work and requires reconciliation; closure as not planned cancels the plan. Reopening a completed parent does not rerun children automatically.

`pause` prevents new turns and delivery. Interrupt an active child through the existing cancellation/stop mechanism at the next available boundary, preserving its branch, worktree, and provider session where supported. An operation already confirmed remotely is reconciled, never blindly reversed. `resume` revalidates dependencies and resumes the interrupted generation rather than creating duplicate work.

`cancel` stops scheduling and owned active execution, preserving results. Resuming a cancelled plan requires an explicit retry/reconciliation action. Project pause has equivalent execution restrictions. Removing the parent label pauses it and prevents subsequent delivery. Pausing/cancelling a parent never closes child issues or discards their work.

A child failure exhausts existing bounded retries before placing the plan in `needs_attention`. Never skip, close, or relabel a failed prerequisite as completed to continue. V1 does not continue other branches of the same plan around a blocked child; other plans remain schedulable.

## 8. Persistence and restart recovery

Extend the existing SQLite store with:

- Supervisor record: parent immutable identity, project, activation origin, state/reason, plan revision/hash, pause/cancel latch, current child, and workpad comment ID.
- Plan revision and child records: issue identity, dependency edges, completion policy, ownership, accepted evidence, and required operational attestations.
- Attempt linkage: supervisor ID, revision, child identity, generation, and dispatch idempotency key.
- Operation/event records: intent/results for enqueue, workpad publication, operator acknowledgements, pause/resume, and evidence invalidation.

Use versioned migration and transactions within the existing store architecture; independent `Put` calls are insufficient for atomic enrollment/claiming. Existing projects and attempts remain valid with supervision disabled.

On startup, reconcile remote issue/PR state, owned attempts, and surviving worker locks before dispatching. Do not replay an acknowledged dispatch, restart awaiting-review work, or lose explicit pauses. Persist state before remote status updates. Workpad errors cannot create a second attempt. On API failure or rate limiting, back off using existing policy, show stale/unknown observations, and retry reconciliation without spending worker slots.

## 9. GitHub and CLI experience

Maintain one parent comment headed `Changathi supervisor`, using a hidden stable plan identifier. Show plan revision, current state, next eligible child or blocker, and a table of child dependencies, delivery PR, review status, and evidence links. Recover the comment by marker after ambiguous creation responses. Retry updates idempotently and edit only Changathi's own comment. Avoid repetitive comments for unchanged polls.

Commands:

```text
changathi supervisor validate homelab#PARENT
changathi supervisor start homelab#PARENT
changathi supervisor list --project homelab
changathi supervisor show homelab#PARENT
changathi supervisor pause|resume|cancel homelab#PARENT
changathi supervisor reload homelab#PARENT [--apply]
changathi supervisor reconcile homelab#PARENT --child 4 --pr PR_NUMBER
changathi supervisor reconcile homelab#PARENT --child 4 --ack-completion --evidence URL
```

`validate` is read-only and reports schema, dependency, ownership, and eligibility problems. All mutations use request IDs and the local daemon API. Structured `--json` responses include blocked child IDs and precise reasons. No generic force/skip gate is included in v1.

The dashboard shows supervisor state separately from implementation agents, e.g. `homelab#PARENT: waiting_review on #4; #5 blocked by #4`. Supervision must not appear as one active worker or conceal available capacity.

## 10. Implementation map

- `internal/core/model.go`: supervisor/revision/child/evidence models, attempt linkage, issue state reason and closure event fields.
- `internal/core/github.go`: labelled-parent discovery, paginated review events, explicit PR evidence lookup, idempotent workpad operations.
- `internal/core/store.go`: migrations, atomic ownership/enqueue operations, persisted supervisor transitions.
- New `internal/core/supervisor.go`: strict plan parsing, graph validation, pure dependency eligibility evaluation.
- New `internal/service/supervisor.go`: reconciliation and child handoff into the existing scheduler.
- `internal/service/engine.go`: shared dependency/parent gates in enqueue, turns, and delivery; exclude parents/owned children from normal automatic pickup. Its current `awaiting_review` logic accepts any closed issue and must not be reused as supervisor completion proof.
- `internal/service/server.go`, `internal/cli/cli.go`, and `internal/cli/client.go`: API, commands, dashboard, and operator reconciliation.

Repository `WORKFLOW.md` continues to configure implementation agents and validation. Parent issue configuration cannot raise host concurrency, weaken sandbox/credential boundaries, or override merge policy. Existing delivery code remains responsible for PR validation and merging.

## 11. Optional supervisor agent

After deterministic supervision is reliable, add bounded advisory turns for proposing graphs, summarizing failures, or assessing missing evidence. Such turns use a separately configured supervisor-agent limit (default disabled), a shared provider/account budget, timeouts, and usage accounting. They release their slot when waiting and never reserve a worker slot while requesting child execution.

The advisory agent submits proposals to the daemon; it cannot bypass dependencies, declare human review complete, close issues, merge PRs, or alter authorization. Graph changes still follow revision/reload rules. Scheduling and restart recovery must remain correct with this feature disabled or its provider unavailable.

## 12. Acceptance tests and rollout

Use fake GitHub/provider adapters and a controllable clock. Required tests:

1. At global/project worker concurrency one, parent starts child A, waits after A's PR merges, then starts B only after A's qualifying human completion. No idle supervisor occupies a slot.
2. `auto: false` does not start a labelled parent; explicit start advances eligible children. `auto: true` discovers a labelled parent once.
3. Ordinary auto/manual dispatch cannot run a parent as a worker or bypass an enrolled child's dependencies. Racing triggers create one generation.
4. Invalid graphs, foreign/missing issues, cycles, duplicated ownership, and conflicting existing attempts fail before any dispatch.
5. Closed-as-not-planned, missing merge, unknown closure reason/actor, bot closure, and unverified required operational outcomes all block advancement.
6. A completed external prerequisite is accepted only through explicit verified evidence association. Human reconciliation is auditable.
7. Parent edits, pause, label removal, cancellation, and project pause stop later turns/delivery; resume preserves unfinished work.
8. A reopened prerequisite before dispatch or during execution blocks its descendants; an already completed merge is surfaced without destructive rollback.
9. Restart after claim, PR publication, merge, workpad timeout, or issue closure preserves exactly one active generation and reconstructs the right wait state.
10. GitHub outages/rate limits never satisfy a dependency from stale data, occupy worker capacity, or cause repeated workpad comments.
11. Completing all children leaves the parent awaiting human review. Premature closure, reopening, and unchanged polling do not cause unrequested execution.
12. Multiple plans/projects remain fair, and unsupervised existing workflows retain their current behavior.

Implement in stages: schema/parser and persistence; read-only validation/status; explicit-start reconciliation and dispatch; lifecycle/recovery tests; automatic discovery and workpad/dashboard; optional advisory agent afterward. Roll out first with a disposable repository and two dependent issues, then a homelab parent. Creating this specification does not start a supervisor or change the existing homelab issues.

## Implementation notes

The controller is deterministic; advisory agent support remains deferred. The authenticated `gh` user is the human operator. `requires_evidence` is an explicit child schema field because operational requirements cannot reliably be inferred from issue text. `closed_completed` children are manual gates and never create code workers.

Reload previews the old/new structured plans. `--apply` requires no active child execution and preserves a paused state; resume is explicit. The initial implementation rejects removal of any dispatched child and policy/dependency changes to a dispatched child. This is stricter than the original invalidation design; append-only expansion with existing edges retained is supported.

Supervisor records and canonical revisions use new kinds in the existing SQLite records store, with schema version 1 in the plan; enrollment and ownership changes share one transaction. Core workers continue through the existing delivery pipeline. Remote reconciliation failures back off up to four minutes; workpad failures retry after two minutes. Reconciled comments use stable markers and journaled publication intent. All remote evidence is checked again before dispatch/delivery; GitHub cannot provide an atomic transaction spanning closure state and merge.

Automated verification uses fake GitHub/provider adapters and local Git repositories. Tests cover actual worker delivery with concurrency one, human completion gates, explicit evidence, plan expansion, ownership races, SQLite reopen, interruption, API failure, comment recovery, and CLI requests. They do not perform real GitHub mutations or live infrastructure changes.


## GitHub evidence approval and attempt supersession

Operational evidence can be approved directly on the child issue after closing it as completed. The daemon posts the exact `/approve-evidence #PR HTTPS_URL` comment to copy, with the current PR and validation report link already filled in. The parent workpad mirrors this action. Only a new, unedited comment by the authenticated human operator after the current closure qualifies. All existing merged-delivery checks remain required. Valid approval releases an evidence-only hold automatically; it does not override explicit pause/cancel or other dependency failures. GitHub comment approvals are revalidated, including deletion or edits, on subsequent reconciliation. CLI acknowledgement remains a fallback.

Old awaiting-review attempts become superseded when replaced by a new generation. Startup repairs historical stale rows. Superseded attempts are inactive, retain audit history, and never consume a worker slot.
