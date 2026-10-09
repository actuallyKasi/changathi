# Changathi shared memory specification

Status: proposed, not implemented, 2026-10-08. Extends [SPEC.md](SPEC.md).

MUST denotes a requirement; SHOULD denotes a recommendation. Defaults and interfaces below are design proposals.

## 1. Purpose

Share durable project knowledge and task handoffs between Changathi-managed agents and independent clients such as desktop Codex. Memory content MUST be provider-neutral so future Claude and other adapters can use it.

Existing execution persistence is not shared knowledge: attempts, messages, events, and provider thread references serve execution and recovery. This feature adds explicit storage, retrieval, and write-back of reusable knowledge. It does not synchronize model internals or merge provider conversation histories.

## 2. Architecture and scope

The Go daemon owns the memory service and remains the sole database writer. Start with dedicated SQLite tables and full-text search on local storage. No LangGraph, vector database, or embedding service is required for the initial implementation.

Memory access has three layers:

- Storage: records, revisions, provenance, and a rebuildable search index.
- Service: scope enforcement, retrieval, conflict detection, and atomic writes.
- Clients: orchestration code, CLI, and an optional MCP adapter using the same service operations.

MCP is an access protocol, not the memory store. Managed agents receive context through their provider adapter's prompt input; tool-capable agents may additionally search and record memories. Desktop access uses the MCP adapter. For a separate host, run the stdio adapter through SSH against the remote daemon's local socket. Direct public HTTP access and database-file synchronization are outside this version.

## 3. Memory records

Each record MUST contain a stable ID, schema version, scope, kind, title, text, revision, status, creation/update timestamps, author metadata, and source references. Optional fields include applicability constraints, expiry, related record IDs, and superseded revision references.

Scopes:

- Personal: explicit preferences that may apply across projects.
- Project: knowledge keyed by Changathi project ID and stable repository identity.
- Task: handoffs keyed by project, immutable issue ID, execution generation, and attempt where available.

Kinds include observation, decision, preference, hypothesis, and handoff. A handoff records completed work, outstanding work, blockers, artifacts, and next steps. Sources may identify an issue, PR, commit, file, validation result, or agent turn. Machine-local paths alone MUST NOT be treated as portable artifact references.

Author metadata distinguishes human, daemon observation, and agent report, with provider/model and run references where available. An agent's claim of success MUST NOT be relabelled as daemon-verified validation. A successful test applies to its recorded commit and environment.

Personal scope MUST require explicit selection; project findings MUST NOT be promoted there automatically. Agents MUST NOT automatically infer and persist personal preferences from incidental conversation.

## 4. Retrieval and context assembly

Before a managed task's initial turn, Changathi MUST assemble a bounded context bundle from its latest applicable handoff, selected personal preferences, and relevant project records. Retrieval MUST filter scope before ranking. Unrelated projects are excluded unless explicitly requested.

Start with exact identifiers, tags, and full-text matching against the task title and description. Prefer applicable decisions and supported observations over hypotheses, while preserving conflicting evidence. Proposed limits are 12 records and 16 KiB of rendered UTF-8 text per initial bundle; truncation MUST preserve character boundaries and identify omitted or shortened content.

Include record IDs, revisions, source references, and applicability alongside content. Expired, superseded, and deleted records MUST be excluded from ordinary retrieval. A commit change does not prove a record false; mark uncertain applicability and require revalidation for claims that depend on that version.

On resumed turns, refresh changed relevant records and handoffs without repeatedly appending unchanged bundles. Record which revisions were supplied to each turn. A running turn does not acquire new context merely because the database changed.

Retrieved text MUST be identified as reference material. It cannot override the user's task, trusted workflow instructions, or delivery checks.

## 5. Write-back and concurrency

Agents SHOULD record durable discoveries when learned and checkpoint at a useful handoff or completed turn. Do not copy every token, tool result, or entire conversation into memory. Unexpected termination preserves the last committed checkpoint; it MUST NOT manufacture a completed handoff.

All mutations require an idempotency key. Updates require the expected revision. The daemon MUST atomically persist the revision, search-index changes, and idempotency result. A repeated key with the same payload returns the original result; a different payload is rejected. Concurrent stale updates return a conflict rather than silently overwriting content.

Corrections create revisions or explicitly supersede a record. Contradictory independent findings remain attributable and discoverable until resolved; recency alone does not establish truth. Agent writes need no per-write human approval within configured scope, but may not silently replace a human-authored preference or decision.

Task state remains authoritative in Changathi's execution store. A memory saying “merged” cannot advance delivery state, close an issue, or satisfy a supervisor prerequisite.

## 6. Proposed interface

The daemon exposes versioned equivalents of:

| Operation | Purpose |
| --- | --- |
| `memory.search(scope, query, limit)` | Return bounded records and revision/source metadata |
| `memory.get(id, revision?)` | Inspect a record or historical revision |
| `memory.record(scope, kind, content, sources, request_id)` | Create a record |
| `memory.update(id, expected_revision, patch, request_id)` | Correct or supersede knowledge |
| `task.get_context(task_id, generation?)` | Assemble a task's memory bundle |
| `task.checkpoint(task_id, generation, handoff, request_id)` | Persist an attributable handoff |

Task identifiers MUST include project identity. Clients MUST distinguish missing memory, empty search results, and service errors. The MCP adapter MUST preserve these semantics and MUST NOT access SQLite directly.

Proposed CLI: `changathi memory search|show|record|update|export|forget`, with explicit scope selection, file-backed content input, and `--json`. A desktop skill or project instructions can explain when to retrieve and save; merely configuring MCP does not guarantee either action occurs.

## 7. Operations

Preserve current single-user socket ownership and SSH authentication boundaries. These provide access control between OS users, not isolation from agents running as the same OS user. Secrets and credentials MUST NOT be persisted as memories or copied into diagnostic output.

Memory retrieval failure SHOULD allow an ordinary coding task to continue with a visible warning. A workflow may explicitly require memory, in which case failure prevents its next turn. Write failures MUST be reported and may be retried with the same idempotency key; they MUST NOT be reported as saved.

Provide schema migrations and consistent SQLite backups. Search indexes MUST be rebuildable from stored records. Explicit forgetting MUST remove content from live records, revisions, and search indexes, retaining only content-free deletion metadata. Document that existing backups and already-supplied agent context are not retroactively erased.

## 8. Acceptance criteria

1. Two provider adapters can consume the same record format and produce handoffs readable by each other; use fake providers until another live adapter exists.
2. A desktop MCP client can retrieve and record memory through an SSH-hosted adapter without opening or copying the database.
3. Starting a task loads relevant scoped memories; another project's records are excluded.
4. A restarted daemon retains memories, revisions, and acknowledged checkpoints.
5. Duplicate requests create one write, and concurrent stale updates report a conflict.
6. Expired/superseded records are excluded by default; historical inspection retains provenance.
7. Context size is bounded, supplied revisions are recorded, and resumed turns receive relevant changes.
8. A hypothesis remains labelled as such, and memory cannot alter delivery or supervisor state.
9. Retrieval and write failures are observable, and abrupt termination preserves the previous checkpoint.
10. Forgetting removes content from live storage and the search index.

## 9. Implementation sequence

1. Record schema, migrations, revisions, scope validation, and CLI inspection/writes.
2. Full-text retrieval and bounded context assembly in the existing Go orchestration path.
3. Explicit agent checkpoints and durable write-back through the service API.
4. MCP adapter, desktop usage instructions, and SSH integration verification.
5. Evaluate retrieval quality on real tasks before adding semantic search or automatic memory extraction.
