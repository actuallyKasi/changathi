# Changathi

Changathi is a personal coding-agent orchestrator. One Go daemon watches GitHub issues across your repositories, runs a coding agent (Codex or Claude) on each one in its own Git worktree, validates the agent's commits, optionally has a second model verify them, opens a PR and merges it when checks pass. The issue then stays open with `agent:review` until you review the result and close it.

You don't drive it by hand. The CLI is built to be operated by Claude Code: install the bundled skill and ask Claude to onboard repositories, write issues, and check on agents. That is separate from the agents that do the issue work, which can be Codex or Claude per repository. You review results and ask for rework on GitHub itself. This README covers installation and how the system fits together; the day-to-day procedures live in the skill ([skills/changathi/SKILL.md](skills/changathi/SKILL.md)).

## Start here

> [!TIP]
> **Let Claude set up your repositories.** Once Changathi is installed, open Claude Code in any repository and say:
>
> **"Onboard this repo to Changathi."**
>
> The bundled skill has Claude work out the validation commands, write and commit `WORKFLOW.md`, register the project, and write issues an agent can finish. You review and close the results.

Installing Changathi is a one-time setup, described in [Getting started](#getting-started).

## Getting started

### 1. Prerequisites

- macOS or Linux
- Go 1.26+, Git
- [GitHub CLI](https://cli.github.com/) (`gh`), logged in
- [Codex CLI](https://github.com/openai/codex) (`codex`), logged in (tested with `codex-cli 0.160.0`), and/or
- [Claude Code](https://claude.com/product/claude-code) (`claude`), logged in (tested with `2.1.296`)

Log in as the OS user that will run the daemon:

```sh
gh auth login
codex login          # for Codex
claude auth login    # for Claude
```

### 2. Build and put it on your PATH

```sh
git clone <this repository> ~/repos/changathi
cd ~/repos/changathi
make build                                   # writes bin/changathi
mkdir -p ~/.local/bin
ln -sf "$PWD/bin/changathi" ~/.local/bin/changathi
changathi doctor                             # checks git, gh and enabled providers are installed and authenticated
```

Because `~/.local/bin/changathi` is a symlink, later rebuilds update it in place.

### 3. Run the daemon as a service

Codex is the only provider until the service configuration enables others. To use Claude, copy [examples/config.yaml](examples/config.yaml) (it allows Claude Opus and Sonnet as well as Codex), delete its `data_dir` line to keep the default location, and pass it to every command below with `--config`:

```sh
changathi daemon install                                        # Codex only
changathi --config ~/.config/changathi.yaml daemon install      # with the providers it lists
changathi --config ~/.config/changathi.yaml doctor --models     # also runs one tiny turn per allowed Claude model
```

`daemon install` writes the service, starts it and waits until the daemon answers. On macOS this writes the launchd agent `~/Library/LaunchAgents/dev.changathi.plist` and loads it; on Linux it writes the systemd user unit `~/.config/systemd/user/changathi.service` and enables it. The service runs the `changathi` on your PATH with the same `--config`, and its `PATH` covers `git`, `gh`, the enabled providers' CLIs (`codex`, `claude`) and the rest of your current shell's `PATH`, so validation commands find the same tools as your terminal. Run install from a shell where those resolve.

It is safe to re-run: if nothing changed it leaves the running daemon alone, otherwise it rewrites the file and restarts the service. Use `--print` to see the file without installing it, and `changathi daemon uninstall` to stop and remove the service (state in the data directory is kept). Logs go to `~/Library/Logs/changathi.log` and `changathi.error.log` on macOS and to `journalctl --user -u changathi` on Linux.

After rebuilding, restart it so it loads the new binary:

```sh
make build && launchctl kickstart -k gui/$(id -u)/dev.changathi   # macOS
make build && systemctl --user restart changathi                 # Linux
```

For a quick try without a service, run `changathi daemon start` in a terminal and leave it open.

### 4. Install the Claude skill

```sh
changathi skill install                      # ~/.claude/skills/changathi/SKILL.md
```

Use `--dir <repo>` to install it into one repository's `.claude/skills/` instead. The skill is embedded in the binary, so reinstall it after each rebuild to keep it in step with the CLI.

### 5. Add your first repository

Open Claude Code in the repository and ask it to onboard the repo to Changathi (see [Start here](#start-here)).

To do it by hand: each repository needs a `WORKFLOW.md` on its **default branch** that tells Changathi how to prove a change is good:

```markdown
---
validation:          # required; run after every agent turn
  - go test ./...
  - go vet ./...
---
Implement issue #{{.Issue.Number}}: {{.Issue.Title}}.
Follow AGENTS.md and existing repository conventions.
```

[examples/WORKFLOW.md](examples/WORKFLOW.md) shows every option, including the `agents` block that picks a provider and model per role, for example Claude Opus to implement and Claude Sonnet to verify. Then register the repository:

```sh
changathi project add backend --repo owner/backend --auto
```

`project add` creates the `agent:ready`, `agent:review`, `agent:rework` and `supervisor` labels and warns if `WORKFLOW.md` is missing. With `--auto`, any open issue labelled `agent:ready` is picked up; without it, issues run only when dispatched with `changathi issue run 'backend#42'`.

### 6. Hand it work

```sh
gh issue create --repo owner/backend --title 'Handle empty input' --body-file issue.md --label agent:ready
changathi summary
```

From here, ask Claude how things are going (it uses `summary` and `agent logs`), or just watch the issue: Changathi comments there when it needs you or has delivered.

## How it works

### Components

```mermaid
flowchart TB
    claude(["Claude + changathi skill"])

    subgraph daemon["changathi daemon"]
        api["Local API<br/>(Unix socket)"]
        sched["Scheduler<br/>+ supervisor"]
        store[("SQLite state")]
        ws["Workspace<br/>manager"]
        runner["Agent runner"]
        deliver["Delivery<br/>controller"]
        api --> sched
        sched --- store
        sched --> ws
        sched --> runner
        runner --> deliver
    end

    subgraph issue["per issue"]
        agent["agent session<br/>(codex app-server or claude -p)"]
        verifier["verifier session<br/>(read-only, optional)"]
        wt["worktree"]
        agent -- "commits" --> wt
        verifier -. "reads" .-> wt
    end

    gh[("GitHub")]

    claude -- "changathi CLI" --> api
    ws --> wt
    runner -- "stdio" --> agent
    runner -- "stdio" --> verifier
    sched -- "poll issues" --> gh
    deliver -- "PR, checks, merge" --> gh
```

- **Local API.** Versioned HTTP over `<data_dir>/daemon.sock`, readable only by your user. The CLI is a thin client; quitting it never stops agents.
- **Scheduler.** Polls GitHub every 30 seconds, round-robins between projects, and enforces global and per-project concurrency (both default to 1). Retry, CI and account-limit waits don't hold a slot.
- **Workspace manager.** Keeps its own clone of each repository, separate from your checkout, and gives every issue a branch and worktree. Worktrees are never deleted automatically.
- **Agent runner.** Starts one provider session per role of an attempt (a `codex app-server`, or `claude -p` speaking stream-json) with a reduced environment and no copied credentials, and streams its events to a per-attempt log. The implement session is resumed across turns and restarts; a verify session is always fresh and read-only.
- **Delivery controller.** Takes the agent's commits, runs validation and verification, publishes the PR and merges it.
- **Supervisor.** Coordinates a parent issue's plan of dependent child issues. It is plain daemon logic and doesn't use an agent slot.
- **State store.** One SQLite database holds projects, attempts, events and journaled GitHub operations, so the daemon can restart without losing or duplicating work.

### Life of an issue

```mermaid
stateDiagram-v2
    [*] --> queued: ready label or issue run
    queued --> preparing: slot free
    preparing --> running: worktree ready
    running --> validating: agent turn ends
    validating --> running: validation fails (next turn)
    validating --> pr_open: commits pass
    validating --> verifying: commits pass, verify role set
    verifying --> running: blocking findings (next turn)
    verifying --> pr_open: verdict pass
    pr_open --> waiting_checks
    waiting_checks --> running: check fails (next turn)
    waiting_checks --> merging: checks green
    merging --> awaiting_review: merged
    awaiting_review --> done: you close the issue
    done --> [*]
```

1. **Pickup.** An issue becomes eligible through the `agent:ready` label (auto projects) or an explicit `issue run`. Eligibility is re-checked against GitHub before dispatch and before delivery, so closing the issue or cancelling it stops the work.
2. **Agent turn.** The `implement` agent (Codex by default) works in the issue's worktree and commits. It is told not to push, open PRs or close issues.
3. **Validation.** Changathi merges the latest base branch, runs the `validation` commands from `WORKFLOW.md`, and requires a clean tree with new commits. A failure becomes the next turn's prompt, up to `max_turns`.
4. **Verification (optional).** With a `verify` role, a separate read-only session (for example Sonnet, while Opus implements) reviews the validated diff against the issue and returns a pass/fail verdict. Blocking findings become the next implement turn's prompt. After `max_verify_rounds` failures, or two malformed verdicts, or if the verifier changes the worktree, the attempt needs attention. Notes from a passing verdict go in the PR body and are posted as a PR review comment.
5. **Delivery.** Changathi pushes the branch, opens `[Changathi] <issue title>`, waits for `required_checks` and GitHub's mergeability, and merges with the validated head SHA pinned. A failing check sends the agent back to work in the same provider thread.
6. **Review.** After merge the issue keeps `agent:review` and stays open. You review and close it; closing is the only human gate.

Anything that can't proceed on its own (exhausted turns, a merge blocked by branch protection, an execution lock still held after a restart) becomes `needs_attention`, and Changathi posts the reason on the issue.

An account limit pauses that provider for five minutes (`waiting_account`) instead of failing the work; a Claude limit on one model's window (such as weekly Opus) pauses only that model. Work never falls back to another model or to API-key billing. A restart or a wait that happens after the agent's turn finished resumes at validation, without another agent turn, and a Claude session that can no longer be resumed continues in a fresh one that is told what the branch already holds.

### Reviewing and reworking on GitHub

You never need to message an agent. Everything happens on the issue:

| You find | You do on GitHub | Changathi does |
| --- | --- | --- |
| It stopped (`needs_attention`) | Comment with guidance if the agent can fix it (or fix the outside cause yourself), then add `agent:rework` | Re-runs the same branch and PR with your comments and the last failure in the prompt |
| It merged but misses the acceptance criteria | Leave the issue open, comment what's missing, add `agent:rework` | Starts a new generation (new branch and PR) that knows the previous PR and your comments |
| You have a question, not a change request | Ask it in your Claude Code session; `agent:rework` isn't needed | Nothing. If you do add `agent:rework`, the agent posts its answer as an issue comment and the issue goes back to review with no new PR |
| It's done, but you spotted a separate bug or idea | Close it as completed; open a new issue that links back | Treats the new issue like any other |

Only your own comments (the `gh` login Changathi runs as) are passed to the agent; comments from anyone else are ignored. `changathi issue retry` does the same as the label from the CLI. For supervisor children, rework before you close the child: its dependants wait for the new delivery.

### Dependent issues

A parent issue labelled `supervisor` holds a plan: which child issues to run and what each depends on. The supervisor dispatches a child once its prerequisites are merged **and** closed by you, so nothing builds on unreviewed work. See [examples/supervisor-issue.md](examples/supervisor-issue.md) for a plan and [SUPERVISOR_SPEC.md](SUPERVISOR_SPEC.md) for the full rules, including evidence approval for operational tasks.

```mermaid
flowchart LR
    p["Parent issue<br/>label: supervisor"] -.plan.-> c1["#3 schema"]
    p -.plan.-> c2["#4 API"]
    p -.plan.-> c3["#5 UI"]
    c1 -- "merged + closed" --> c2
    c2 -- "merged + closed" --> c3
```

### Trust model

Worktrees isolate Git changes, not processes. Implementing agents run unattended as the daemon's OS user (Codex with `approvalPolicy=never`, `sandbox=danger-full-access`; Claude with `--permission-mode bypassPermissions`), and validation commands run as that user too. Verifiers run read-only (Codex `sandbox=read-only`; Claude in `dontAsk` mode with only read tools and an allowlist of read-only shell commands such as `git ls-files` and `grep`, and no project settings or hooks from the issue branch), and Changathi stops any attempt whose verifier changed the worktree anyway. Only register repositories whose code and workflow commands you trust. `WORKFLOW.md` is always read from the base branch, so an agent can't change its own policy by editing it. GitHub branch protection still applies; Changathi never bypasses required reviews.

## State and configuration

Everything lives under `~/.local/share/changathi`:

| Path | Contents |
| --- | --- |
| `state.db` | SQLite: projects, attempts, events, operations |
| `daemon.sock`, `daemon.lock` | API socket and single-daemon lock |
| `repos/<project-id>` | Changathi's own clone of each repository |
| `worktrees/<project-id>/<attempt-id>` | One worktree per execution |
| `logs/<attempt-id>.log` | Agent transcript, rotated at 16 MiB |

To change the data directory, poll interval, global concurrency, provider paths, or which providers and models are allowed (Claude is off until it's listed under `providers`), pass `--config` with a YAML file like [examples/config.yaml](examples/config.yaml); the CLI and daemon must agree on `--data-dir`. Keep the data directory on local disk, and don't point two daemons at the same repositories.

Worktrees are kept after failures. `changathi workspace clean <agent-id>` previews removal of a delivered worktree, and `--apply` removes it.

## Development

```sh
make test                                    # go test -race ./...
make vet
CHANGATHI_CODEX_SMOKE=1 go test ./internal/provider -run TestInstalledCodexHandshake -v
CHANGATHI_CLAUDE_SMOKE=1 go test ./internal/provider ./internal/service -run 'TestInstalledClaude|TestLiveClaude' -v
```

The Codex smoke test checks the protocol handshake without running a model turn. The Claude smoke tests spend a few small turns: one verifier turn through the adapter (plus a check that resuming a missing session is detected), and an Opus-implements/Sonnet-verifies run against a local repository with GitHub faked. Automated tests use a fake provider and never touch real GitHub repositories.

| Package | Role |
| --- | --- |
| `cmd/changathi` | Entry point |
| `internal/cli` | Cobra commands, compact output, skill install |
| `internal/service` | Daemon: API server, scheduling and delivery engine, supervisor |
| `internal/core` | Models, SQLite store, GitHub (`gh`) adapter, workflows, worktrees |
| `internal/provider` | Provider interface, Codex app-server and Claude Code (stream-json) adapters |
| `internal/transcript` | Turns provider events into readable logs |
| `skills/changathi` | The embedded Claude skill |

## Limits and further reading

GitHub.com only, one repository per project, and the daemon is reached locally or over SSH. [SPEC.md](SPEC.md) is the full design with notes on where the implementation differs, [SUPERVISOR_SPEC.md](SUPERVISOR_SPEC.md) covers dependent issues, [CLAUDE_PROVIDER_SPEC.md](CLAUDE_PROVIDER_SPEC.md) covers the Claude provider and model roles, and [SHARED_MEMORY_SPEC.md](SHARED_MEMORY_SPEC.md) is a proposal that isn't implemented yet.
