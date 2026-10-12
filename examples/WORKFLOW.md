---
validation:
  - go test ./...
  - go vet ./...
required_checks: []
merge_method: squash
max_turns: 5
timeout_seconds: 3600
concurrency: 1
# Optional: provider and model per role, replacing the top-level `model` key.
# Every role must be allowed by the service configuration's `providers` list.
# agents:
#   implement:             # writes and fixes the code
#     provider: claude
#     model: claude-opus-5-5
#     effort: high         # claude only: low | medium | high | xhigh | max
#   verify:                # read-only review of each validated head before the PR
#     provider: claude
#     model: claude-sonnet-5-5
#     effort: medium
#     instructions: Block on missing tests for changed behavior.
# max_verify_rounds: 2     # failing verdicts before the attempt needs attention
---
Implement issue #{{.Issue.Number}}: {{.Issue.Title}}.
Follow repository conventions and AGENTS.md. Add meaningful tests for changed behavior.
