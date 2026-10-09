---
validation:
  - go test ./...
  - go vet ./...
required_checks: []
merge_method: squash
max_turns: 5
timeout_seconds: 3600
concurrency: 1
---
Implement issue #{{.Issue.Number}}: {{.Issue.Title}}.
Follow repository conventions and AGENTS.md. Add meaningful tests for changed behavior.
