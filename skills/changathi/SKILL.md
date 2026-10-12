---
name: changathi
description: Drive the Changathi coding-agent orchestrator. Use when onboarding a GitHub repository to Changathi, writing issues for its agents, coordinating dependent issues with a supervisor plan, or checking on, reworking or stopping agents.
---

# Changathi

Changathi is a local daemon that runs coding agents (Codex or Claude) on GitHub issues. Each issue gets its own worktree. When the agent commits, Changathi runs the repository's validation, opens a PR and merges it once checks pass. The issue then stays open with `agent:review` until a human closes it.

Use the `changathi` CLI to talk to the daemon, and `gh` for anything that only touches GitHub, such as creating or editing issues. Add `--json` when you need to parse output. The default text output is compact tables, which are usually cheaper to read.

Start with `changathi summary`. It reports what is running, what needs attention and what is awaiting review in a single call. Don't use `daemon status --json` to check on work, because it dumps all daemon state.

## Before anything else

```sh
changathi doctor            # git, gh and enabled providers present and authenticated (--models probes each Claude model)
                            # pass the daemon's --config too, or doctor checks Codex only
changathi summary           # fails with "cannot reach daemon" if it isn't running
```

If the daemon isn't running, offer `changathi daemon install`: it sets the daemon up as a launchd (macOS) or systemd (Linux) service, starts it and waits until it answers, and is safe to re-run. Ask before installing the service. Don't run `changathi daemon start` yourself in a shell that will exit.

Check which providers the running daemon actually has before you onboard or dispatch anything:

- The service's command line shows its config (`ps -o command= -p $(pgrep -f 'changathi daemon start')`). With no `--config`, or a config without a `providers` block, the daemon is Codex only, whatever `WORKFLOW.md` says.
- A daemon started before the binary was rebuilt keeps running the old code. If the daemon is older than the binary, or the source checkout has newer commits than the binary, rebuild and re-run `daemon install` with the same `--config`, then `changathi skill install`. Ask first, because this restarts running work.

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

   To use Claude, add an `agents` block (instead of `model`). A typical setup is Opus to implement and Sonnet to verify:

   ```yaml
   agents:
     implement: {provider: claude, model: claude-opus-5-5, effort: high}
     verify: {provider: claude, model: claude-sonnet-5-5, effort: medium}
   ```

   The verifier reviews each validated head read-only. Its blocking findings go back to the implementer, and its notes are posted on the PR as a review comment. Every role's provider and model must be on the daemon's `providers` allow-list (see `examples/config.yaml`); otherwise the project shows a workflow error and nothing dispatches. Ask the user before choosing models, since they decide the cost. Settle the provider before any issue is dispatched, because each attempt keeps the `WORKFLOW.md` it was first dispatched with. A later change to the base branch reaches an existing attempt only through `issue retry`, which re-reads it.

   The body is a Go `text/template` that receives `.Issue`. Keep it short, because Changathi already adds the issue text and delivery rules to the prompt.
3. Register the repository:

   ```sh
   changathi project add <name> --repo owner/repo [--auto] [--concurrency N]
   ```

   This creates the `agent:ready`, `agent:review`, `agent:rework` and `supervisor` labels and warns if `WORKFLOW.md` is missing on the base branch. Fix any warning before you dispatch work. `--auto` picks up open issues labelled `agent:ready` and parents labelled `supervisor`. Without it, issues run only when you call `issue run`. Ask the user which one they want if they haven't said.

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

## Check in

```sh
changathi summary                              # start here
changathi agent list --active                  # compact rows with the latest verdict; --project P, --limit N
changathi agent show <id>                      # one agent: roles, latest verdict; no workflow or issue body (--full for all)
changathi agent logs <id> --last               # the agent's last message and what followed
changathi agent logs <id> --tail 20            # last 20 transcript items; --full adds tool output
changathi issue retry '<project>#<number>'     # rework: same as adding agent:rework on GitHub; re-reads WORKFLOW.md
changathi issue cancel '<project>#<number>'    # stop the active attempt for an issue
changathi project pause|resume <name>
changathi usage                                # Codex account limits; Claude tokens and cost per model, last rate-limit window
```

When an agent shows `needs_attention`, read `agent logs <id> --last` and say why it stopped before you retry it. A blocked merge usually means a failing required check or a branch-protection rule. "verification failed N times" means the verifier kept finding blocking problems; its findings are in the logs and in `agent show`. A `waiting_account` agent is paused on a provider or model limit and resumes on its own. The same reason is posted as a comment on the issue.

There is no way to message a running agent. The user reviews and asks for rework on GitHub:

- **Rework of the same issue** (it stopped, or it merged but misses the acceptance criteria): they comment on the issue with what to change, then add the `agent:rework` label. Changathi removes the label, re-queues the issue (a new generation with a new PR if the old one merged) and passes their comments to the agent. Only comments by the `gh` login Changathi runs as reach the agent. If they ask you to do this, draft the comment for them to post or approve; don't post it as them unasked.
- **A question, not a change request** ("do we need to handle X?"): this doesn't need `agent:rework`. Answer it yourself from the code, the issue, the PR and `agent logs <id>`. Use rework only when the user wants code changed. If a question does go through `agent:rework`, the agent is told to answer without changing code. Changathi then posts its answer on the issue as an "agent reply" comment and the attempt goes back to `awaiting_review` (the earlier merged PR stays the delivery), with no validation, verification or new PR. If there was no merged generation before it, the attempt goes to `needs_attention` instead. The reply is also in `agent logs <id>` as a `reply` event. If the turn changes the branch anyway, the reply is ignored and the change is validated and delivered as usual.
- **A separate bug or follow-up idea**: a new issue that links back. The original is closed as completed once its own criteria are met.
- **Supervisor children**: rework before the child is closed, so its dependants wait for the new delivery. Never close and reopen a child to trigger rework.

## Leave to the human

- Closing issues. Closure is the human review gate, so don't close issues for them with `gh` either.
- Evidence acknowledgements for supervisor children.
- Cleaning worktrees (`changathi workspace clean <id> --apply`), unless they ask.
