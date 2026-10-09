# Coordinate the homelab Kubernetes implementation

Advance through the seven implementation issues in order. Each child must have its change merged and be closed as completed after human review. For infrastructure issues, link operational validation before acknowledging completion.

```changathi-supervisor
version: 1
children:
  - issue: 3
    depends_on: []
    completion: merged_and_closed
  - issue: 4
    depends_on: [3]
    completion: merged_and_closed
    requires_evidence: true
  - issue: 5
    depends_on: [4]
    completion: merged_and_closed
    requires_evidence: true
  - issue: 6
    depends_on: [5]
    completion: merged_and_closed
    requires_evidence: true
  - issue: 7
    depends_on: [6]
    completion: merged_and_closed
    requires_evidence: true
  - issue: 8
    depends_on: [7]
    completion: merged_and_closed
    requires_evidence: true
  - issue: 9
    depends_on: [8]
    completion: merged_and_closed
    requires_evidence: true
```

This is a parent-issue template, not a dispatch request. Apply the `supervisor` label when creating the parent. Respect each child's live-application authorization requirements. For `requires_evidence`, the operator acknowledges completion with an HTTPS evidence link using the CLI after the child is closed as completed.
