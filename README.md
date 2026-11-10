# DDE release fixes: isolated regression tests

This repository tests the current local fixes against real GitHub PRs, statuses, Actions concurrency and branch protection. Source snapshots and SHA-256 hashes are in `sources/` and `source-hashes.json`; changes relative to each base commit are in `patches/`.

The regression tests check that a shared `queue: max` serializes baseline status writers while retaining multiple queued PR checks, that an empty-body HTTP 503 produces a failing member status and nonzero exit code, and that action-sync updates protected targets through reusable PRs while reporting direct push failures.

Only isolated fixture branches are merged. The temporary deploy key and secret are revoked after testing.
