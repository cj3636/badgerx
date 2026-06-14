# Git Audit Mirror

Git export is optional and non-authoritative. BadgerDB remains the source of truth. Git and Soft Serve are audit mirrors only; they are not database replication or synchronization.

## Filesystem and Git exporters

`FilesystemExporter` writes JSON version files under `history/{encodedKey}/` and operation files under `ops/`. `GitExporter` writes the same files in a Git worktree and can commit each exported operation.

Database writes do not depend on export success unless strict export mode is explicitly enabled. Failed exports are recorded durably in operation records, indexed by export state, and can be listed or retried. The CLI `badger git export` command retries pending exports and, by default, failed exports.

## Status and push

`GitExporter.GitStatus` reports whether the worktree is initialized, whether it is clean, the current branch, HEAD commit, configured remote name and URL, and pending export count. Ahead/behind values are currently best-effort placeholders and may be zero when no upstream comparison is performed.

`GitExporter.GitPush(remote, branch)` pushes the audit mirror through normal Git. If no remote is configured, it returns a typed remote-not-configured error. Git output is redacted to avoid exposing URL credentials.

## Soft Serve

`SoftServeExporter` validates obvious SSH remote shapes, can configure a Git remote, and tests availability with `git ls-remote` when practical. It does not manage Soft Serve servers and does not use Git as a database synchronization engine.
