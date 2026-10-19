# Coordinate the homelab Kubernetes implementation

Advance through the seven implementation issues in order. Each child must have its change merged and be closed as completed after human review. Infrastructure children deliver a committed validation report.

<!--
The plan is not written in this body. Set it up with GitHub's issue relationships:

- Label this parent `supervisor`.
- Add #3–#9 as its sub-issues, in order (the order breaks ties between ready children).
- Mark each child "blocked by" the one before it: #4 by #3, #5 by #4, … #9 by #8.
- Label #4–#9 `changathi:evidence`: each PR must commit one report under docs/validation/
  (or carry an "Evidence: https://…" line). Label a child `changathi:manual` for a
  review-only gate with no agent.

With the gh CLI (the API takes issue IDs, not numbers):

  id() { gh api repos/OWNER/REPO/issues/$1 --jq .id; }
  gh api repos/OWNER/REPO/issues/10/sub_issues -F sub_issue_id=$(id 4)
  gh api repos/OWNER/REPO/issues/4/dependencies/blocked_by -F issue_id=$(id 3)
  gh issue edit 4 --repo OWNER/REPO --add-label changathi:evidence
-->

This is a parent-issue template, not a dispatch request. Respect each child's live-application authorization requirements. Review each delivered child and its evidence, then close it as completed; that closure is the approval that lets its dependants start.
