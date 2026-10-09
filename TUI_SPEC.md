# Changathi interactive terminal specification

Status: implemented baseline, reconciled with the code on 2026-10-09. Outstanding limitations and unverified acceptance work are listed in §9. Extends [SPEC.md](SPEC.md) and accommodates [SUPERVISOR_SPEC.md](SUPERVISOR_SPEC.md).

## 1. Goal and current behavior

Make agent output comfortable to read while monitoring and interacting with multiple coding jobs. Prioritize a coherent transcript, navigation, and stable scrolling.

The Cobra CLI provides readable command output by default and machine-readable output with `--json`. In a capable terminal, `watch` opens the Bubble Tea dashboard and `agent view` opens a focused transcript. Noninteractive `watch` polls status every two seconds; formatted `agent logs` provides append-only output. Normalized provider events assemble streamed text into messages.

## 2. Framework research and recommendation

Sources inspected on 2026-10-08. Assessments of fit are design judgments, not adoption rankings.

| Candidate | Strength | Fit for Changathi |
| --- | --- | --- |
| [Bubble Tea](https://github.com/charmbracelet/bubbletea) with [Bubbles](https://github.com/charmbracelet/bubbles), [Lip Gloss](https://github.com/charmbracelet/lipgloss), and [Glamour](https://github.com/charmbracelet/glamour) | Go event-driven UI, reusable controls, styling/layout, Markdown rendering | Recommended; preserves the Go binary and supports a custom transcript interface |
| [tview](https://github.com/rivo/tview) | Go widgets including tables, trees, forms, and text views | Credible alternative for a conventional operations dashboard |
| [Ink](https://github.com/vadimdemedes/ink) | React-based terminal interfaces; its README lists Claude Code and Gemini CLI among users | Strong option for a TypeScript frontend, but adds another runtime/build stack here |
| [Ratatui](https://ratatui.rs/) | Rust terminal UI library | Good option in a Rust application; requires a separate client implementation here |
| [Textual](https://textual.textualize.io/) | Python application framework for terminal interfaces | Useful for Python tooling; introduces Python packaging here |

[Crush](https://github.com/charmbracelet/crush) is a relevant coding-agent UI reference. Its [dependency manifest](https://github.com/charmbracelet/crush/blob/main/go.mod) includes the Charm UI libraries. Glamour's README lists Glow, GitHub CLI, and GitLab CLI as users of its Markdown rendering.

Retain Cobra for commands and flags. Use Bubble Tea for the interactive screen, Bubbles for list/viewport/input/help controls, Lip Gloss for a consistent theme, and Glamour for Markdown. The implementation pins Bubble Tea v2.0.9, Bubbles v2.2.1, Lip Gloss v2.0.6, and Glamour v2.0.1 using `charm.land` v2 module paths. Bubbles supplies viewport and inputs; issue rows and help are custom views. No orchestration framework change is needed.

## 3. Entry points and interaction

- `changathi watch`: interactive dashboard when connected to a capable terminal; preserve noninteractive status output and `--json`.
- `changathi agent view <attempt-id>`: open a focused transcript using the same UI components.
- `changathi agent logs <attempt-id> --follow`: retain an append-only view suitable for terminal scrollback and pipes; offer readable messages with explicit raw-event access.

At 90 terminal columns or wider, the dashboard has a left supervisor/issue column and a right output pane. The left column occupies 40% of terminal width, bounded to 42–64 columns. Its pinned supervisor summary contains the issue number, wrapped title, status, and a plain-text description preview. The summary is limited to eight lines, reduced on short terminals to reserve child-list space; truncated text ends with an ellipsis. The full description is available through `supervisor show`.

The supervisor is a group heading, not a selectable agent row. Only that supervisor's children appear by default. Each child shows its issue number, subject, progress badge, and agent activity badge. Issue open/closed badges appear when supported by current supervisor evidence. Previous attempts are hidden until history is enabled. Standalone agents have a separate group; focused `agent view` remains available for any attempt.

Selecting a child with an attempt opens its transcript. A child without an attempt shows its own subject, status, reason, and dependency subjects/statuses; it does not repeat the supervisor overview. Preserve selection and transcript scroll position across status refreshes. The right pane follows the selected child while the supervisor stays visible on the left. The top application header contains the selected issue reference and navigation/output controls; it does not repeat the issue subject, progress, issue open/closed state, or agent activity badges already shown below.

Below 90 columns, use a single list or output pane with Tab/Enter/Esc navigation. The supervisor number, status, and title remain above that pane; the description preview belongs to the wide layout.

| Control | Behavior |
| --- | --- |
| ↑/↓ or j/k | Select children when the list has focus; scroll output otherwise |
| ←/→ | Switch supervisor groups and standalone agents |
| h | Show/hide previous attempts in the selected supervisor group |
| Mouse wheel | Navigate rows over the child list; scroll output over the output pane |
| Tab / Enter / Esc | Change focus / open output or expand a tool / return to list or leave input |
| [ / ] | Select previous/next transcript item |
| / | Search loaded transcript items |
| p | Load older retained output |
| f / End / G | Resume following the bottom |
| m / Ctrl-S | Open composer / send, subject to eligibility checks |
| r / e | Toggle raw inspection / export loaded transcript as Markdown |
| s / t | Stop agent / retry issue, with Enter confirmation and Esc cancellation |
| ? / q | Help / quit viewer without stopping daemon work |

Wheel input over headers is ignored. Modal input/help owns interaction; ordinary typed characters MUST NOT trigger global shortcuts. Mouse selection does not send messages or mutate tasks.

### Noninteractive command output

Human-readable output is the default even when redirected. `supervisor list` is a table with subject, status, completed-child count, current child, and detail. `supervisor show <project>#<parent>` shows the parent description and a child table with subjects, statuses, dependencies, PRs, and reasons. Project, issue, and agent details, workspace lists, usage, doctor, and action responses also have readable defaults; generic nested results use deterministic labeled text. `--json` preserves the returned data structure. Log commands emit individual event JSON with `--json`; `--raw` requests readable individual events.

## 4. Transcript presentation

Group provider output into distinct operator messages, assistant messages, tool calls/results, validation results, and lifecycle changes. Render assistant Markdown with code highlighting and width-aware wrapping. Keep commands and stdout/stderr in literal code/log blocks rather than interpreting them as assistant Markdown.

Tool entries show a concise action, running/completed/failed state, duration when known, and an expandable result. Failed results SHOULD expand by default. Successful repetitive tool output SHOULD remain collapsed. Render diffs with additions/deletions distinguishable without relying on color alone. Preserve source text for export and raw inspection.

Each assistant message streams into one block. Batch redraws and cache completed Markdown; do not reparse the entire transcript for every token. Incomplete Markdown MUST remain readable, with a final render when the message completes. Glamour provides Markdown rendering; incremental assembly and streaming behavior remain Changathi responsibilities.

Auto-follow only while the user is at the bottom. Scrolling upward pauses follow and shows a new-output count. An explicit end/follow action returns to live output. Resize reflows content while preserving a logical message anchor.

## 5. Event and history contract

Use the implemented versioned normalized event envelope carrying sequence, project/attempt, provider thread/turn, item/tool-call identity, timestamp, kind, and typed payload. Message deltas and final snapshots MUST be distinguishable. Completion must reconcile accumulated text instead of appending the final text a second time.

Persist enough item structure to replay the transcript after restart. Unknown events remain available in a raw view and MUST NOT crash rendering. Existing text-only history preserves individual events; unavailable item boundaries MUST NOT be invented. Consequently, older `item/agentMessage/delta` records without item identity may still appear as separate word/token fragments with protocol headings. Restarting the daemon does not reconstruct that history. This is a known readability limitation, not the presentation used for normalized messages.

History retrieval MUST support attempt filtering, bounded pages, and sequence cursors. Reconnection deduplicates by sequence and explicitly reports expired history/gaps. Initial history plus live updates must not lose events at the transition. The daemon remains authoritative; the TUI is a client.

Network calls run asynchronously outside the UI update path. Bound event queues, rendered caches, and resident history. Coalesce paints rather than dropping transcript data; load older pages on demand. Retention gaps are visible. Start with cursor polling if appropriate; server-push transport can follow independently.

## 6. Messaging and feedback

The composer sends through the existing durable message API and displays queued, sent, acknowledged, failed, or uncertain status as reported by the service. It MUST NOT claim that an agent saw a message simply because the request was accepted. Reconnects must not duplicate a submitted message; retain unsent drafts per attempt.

Before opening the composer and again before submitting, check the latest status snapshot. Only `preparing` and `running` attempts can be messaged from the TUI. No attempt, unknown status, an unavailable daemon, inactive states, and delivery phases (`validating`, `pr_open`, `merging`) block submission with an explanation. If eligibility changes while composing, retain the draft and do not issue the send request. The daemon still validates requests; polling leaves a possible race between the UI check and submission. These UI restrictions do not change the durable API's queueing semantics for other clients.

Expose retry/stop actions with clear target context using existing daemon semantics. Viewing, filtering, changing focus, and quitting MUST NOT trigger task mutations.

## 7. Terminal behavior

Support light/dark backgrounds, reduced-color terminals, `NO_COLOR`, and a plain noninteractive fallback. State MUST be legible without color; special fonts and emoji are optional. Bound long lines and use Unicode display widths rather than byte slicing.

Sanitize untrusted terminal control sequences before display, retaining intentional newlines/tabs and generating styles within the renderer. Raw inspection still escapes terminal controls. Markdown export works from source text, not screen escape sequences. Clipboard controls are not implemented.

Restore terminal state and cursor on exit or error. Reconnect with visible backoff/status rather than closing immediately when the daemon becomes unavailable. Test macOS and Linux terminals, including SSH and tmux. Mouse-wheel controls are implemented and MUST preserve keyboard operation. Display daemon transport failures once in a compact footer (`Daemon unavailable · retrying · run: changathi daemon start`), not as repeated raw socket errors at the top and bottom. Waiting transcript content uses a neutral loading message. Existing output remains readable; activity becomes unknown and messaging is blocked until status reconnects. The viewer does not automatically start the daemon.

## 8. Delivery and acceptance

1. Normalize/store/replay agent messages and tool events; verify split deltas, interleaved calls, final snapshots, and legacy history.
2. Add a styled transcript to the existing log command, preserving pipes and machine output.
3. Build the Bubble Tea dashboard and focused transcript with search, expansion, and stable scrolling.
4. Add the composer, supervisor navigation, reconnect behavior, and accessible themes.

Acceptance includes keyboard-only navigation; accurate transcript replay without duplicate text; responsive streaming while other agents run; scroll position surviving new output and resize; bounded memory on long sessions; visible history gaps; accurate message-delivery states; and a viewer exit that does not stop agents.

Use event replay fixtures and focused UI state tests, plus manual terminal checks at wide/narrow sizes and light/dark/no-color settings. Include malformed Markdown, large tool output, ANSI control input, multibyte text, disconnect/reconnect, and final-snapshot duplication cases. Pin a representative large-transcript fixture and record render/input latency before setting a performance target.

## 9. Implementation boundaries and remaining work

The Charm dashboard, focused `agent view`, formatted logs, transcript event payloads, filtered cursor API, message composer, history paging, raw inspection, Markdown export, and stop/retry controls are implemented. Status uses two-second polling; transcript polling runs between status refreshes and backs off on errors. Legacy records preserve actual event boundaries.

The append-only log view emits completed blocks while following; the interactive viewer displays live deltas. Export covers loaded transcript items. Per-item text is limited to 64 KiB, assembled transcripts to 500 items / approximately 2 MiB of source text, and resident event history per attempt to 2,000 events / approximately 4 MiB of event text. Transcript history caches target six resident attempts, with busy/current histories protected during trimming. Drafts and uncertain-submission metadata are retained even when a history cache is released. Retention and viewer truncation are visible. Drafts last for the current client session.

Live transcript caches are refreshed after a daemon restart through the same cursor API. A cursor referring to deleted history or a replaced database produces a gap indicator. A persistent feed ID identifies database replacement. This is a single-daemon history feed; clipboard controls and server-push transport remain follow-up improvements. Mouse-wheel scrolling navigates issues over the list and scrolls output over the transcript. Scrolling up pauses following; reaching the bottom resumes it.

### Status language (implemented)

The viewer separates execution activity, work progress, and issue state. “Agent active” means the daemon reports preparing, running, validating, publishing a PR, or merging. Queued, retrying, waiting, and terminal attempts show “Agent inactive”; this does not mean they cannot resume. Children without an attempt show “No agent yet”. Supervisors are coordination records and do not receive worker activity badges. An unavailable status feed shows “Agent status unknown”.

Progress labels use user-facing language: pending → Not started, awaiting_review → Ready for review, superseded → Replaced, and done → Work finished. A supervisor child that passed its completion gate shows Completed. Issue open/closed badges use supervisor polling evidence, not the issue snapshot captured when a worker started. Unknown issue state is omitted. “Auto-scroll on” describes the viewer following output and does not imply a running agent.

### Remaining limitations and verification

- Old daemons without the transcript API require a restart after upgrade; there is no old-API compatibility fallback. CLI-only layout changes require reopening the viewer, not restarting the daemon.
- The supervisor description is a bounded preview, not an independently scrollable Markdown pane. Use `supervisor show` for the full body.
- Old text-only streaming events cannot be reliably regrouped into original messages. They remain individually visible.
- Clipboard actions, server-push streaming, and shared memory across separate daemons are not implemented. See [SHARED_MEMORY_SPEC.md](SHARED_MEMORY_SPEC.md) for the separate memory proposal.
- A saved supervisor workpad error may remain visible after network recovery when the progress-comment body has not changed. Error freshness/clearing needs a service fix; the CLI currently displays the stored error faithfully.
- CLI tests cover transcript behavior, mouse routing/follow, layout bounds, supervisor navigation, activity states, inactive-message guards/draft retention, and readable versus JSON output. macOS and Linux binaries have been built. This does not establish the full SSH/tmux/light/dark terminal acceptance matrix or measured large-transcript latency; those checks remain follow-up validation.
