---
name: changathi
description: Drive the Changathi coding-agent orchestrator. Use when onboarding a GitHub repository to Changathi, writing issues for its agents, coordinating dependent issues with a supervisor plan, or checking on, steering, retrying or stopping running agents.
---

# Changathi

Changathi is a local daemon that runs Codex agents on GitHub issues. Each issue gets its own worktree. When the agent commits, Changathi runs the repository's validation, opens a PR and merges it once checks pass. The issue then stays open with `agent:review` until a human closes it.

Use the `changathi` CLI to talk to the daemon, and `gh` for anything that only touches GitHub, such as creating or editing issues. Add `--json` when you need to parse output. The default text output is compact tables, which are usually cheaper to read.

Start with `changathi summary`. It reports what is running, what needs attention and what is awaiting review in a single call. Don't use `daemon status --json` to check on work, because it dumps all daemon state.

## Before anything else

```sh
changathi doctor            # git, gh, codex present and authenticated
changathi summary           # fails with "cannot reach daemon" if it isn't running
```

If the daemon isn't running, ask the user to start it (`changathi daemon start` runs in the foreground, or through its launchd/systemd unit). Don't start it yourself in a shell that will exit.

## Onboard a repository

1. Read the repository and work out the commands that prove a change is good: build, tests, lint, vet. They must run unattended and fail loudly.
2. Write `WORKFLOW.md` at the repository root and commit it to the **default branch**. Changathi only reads it from the base branch, so a copy on a feature branch has no effect:

   ```markdown
   ---
   validation:            # required, at least one command
     - go test ./...
     - go vet ./...
   required_checks: []    # GitHub check names that must pass before merge
   merge_method: squash   # squash | merge | rebase
   max_turns: 5
   timeout_seconds: 3600
   concurrency: 1
   ---
   Implement issue #{{.Issue.Number}}: {{.Issue.Title}}.
   Follow AGENTS.md and existing repository conventions.
   ```

   The body is a Go `text/template` that receives `.Issue`. Keep it short, because Changathi already adds the issue text and delivery rules to the prompt.
3. Register the repository:

   ```sh
   changathi project add <name> --repo owner/repo [--auto] [--concurrency N]
   ```

   This creates the `agent:ready`, `agent:review` and `supervisor` labels and warns if `WORKFLOW.md` is missing on the base branch. Fix any warning before you dispatch work. `--auto` picks up open issues labelled `agent:ready` and parents labelled `supervisor`. Without it, issues run only when you call `issue run`. Ask the user which one they want if they haven't said.

## Write an issue an agent can finish

An agent reads only the issue and the repository, so put everything it needs in the issue body:

- **Goal**: one or two sentences on the outcome, not the implementation.
- **Scope**: the files, packages or endpoints involved, and anything out of bounds.
- **Acceptance criteria**: a checklist of observable behaviour, including edge cases.
- **Validation**: which tests to add or update. The workflow's validation commands run anyway.

Keep each issue to one PR's worth of work. Split anything larger and coordinate the pieces with a supervisor plan.

```sh
gh issue create --repo owner/repo --title "..." --body-file issue.md --label agent:ready   # auto projects
gh issue create --repo owner/repo --title "..." --body-file issue.md
changathi issue run '<project>#<number>'                                                  # manual dispatch
```

## Coordinate dependent issues (supervisor)

1. Create every child issue first, without `agent:ready`. The supervisor dispatches children itself when their dependencies are met.
2. Create the parent issue with the `supervisor` label and exactly one fenced plan block:

   ````markdown
   Goal of the whole effort, in prose.

   ```changathi-supervisor
   version: 1
   children:
     - issue: 3
       depends_on: []
       completion: merged_and_closed   # default; closed_completed = manual gate, no agent
     - issue: 4
       depends_on: [3]
       requires_evidence: true          # human must attach an HTTPS evidence URL
   ```
   ````

   Children must be issues, not PRs, in the same repository. Dependencies must not form cycles, and a child can't carry the `supervisor` label.
3. Validate the plan, then start it. Auto projects start it on their own.

   ```sh
   changathi supervisor validate '<project>#<parent>'
   changathi supervisor start '<project>#<parent>'
   changathi supervisor show '<project>#<parent>'
   ```

A child counts as complete only when its PR is merged **and** a human closes the issue as completed. If the parent body changes, run `supervisor reload` and then `reload --apply` (the plan stays paused), followed by `supervisor resume`. Work that was done outside Changathi is accepted with `supervisor reconcile --child N --pr M`. For `requires_evidence` children, the human reviews the evidence, closes the child and posts the `/approve-evidence #PR URL` comment that Changathi suggests on it. `supervisor reconcile --child N --ack-completion --evidence <url>` is the CLI fallback. Never post approvals, make up an evidence URL or acknowledge completion on the user's behalf.

## Check in and steer

```sh
changathi summary                              # start here
changathi agent list --active                  # compact rows; --project P, --limit N
changathi agent show <id>                      # one agent, no workflow or issue body (--full for all)
changathi agent logs <id> --last               # the agent's last message and what followed
changathi agent logs <id> --tail 20            # last 20 transcript items; --full adds tool output
changathi agent message <id> --text "..."      # steers the current turn or queues for the next
changathi issue retry '<project>#<number>'     # rerun unfinished or failed work, keeping its worktree
changathi issue cancel '<project>#<number>'    # stop the active attempt for an issue
changathi project pause|resume <name>
changathi usage                                # provider account limits
```

When an agent shows `needs_attention`, read `agent logs <id> --last` and say why it stopped before you retry it. A blocked merge usually means a failing required check or a branch-protection rule. A message marked `uncertain` may or may not have reached the agent. Check before you resend it.

## Leave to the human

- Closing issues. Closure is the human review gate, so don't close issues for them with `gh` either.
- Evidence acknowledgements for supervisor children.
- Cleaning worktrees (`changathi workspace clean <id> --apply`), unless they ask.
