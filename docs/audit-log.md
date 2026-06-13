# Audit Log

Every versioned mutation writes an immutable `OperationRecord`. Operation records are the Phase 1 audit trail and are intended to become the input for future synchronization work, but this phase does not implement networking, replication, consensus, or conflict resolution.

## Operation records

An operation record contains an operation ID, type, logical key, resulting version ID, timestamp, and host ID. The operation ID is unique and stable, allowing external tools to correlate operation records with version records.

## Audit API

`Audit(key)` scans operation records for the logical key in timestamp order. The operation log is append-only: update, delete, and rollback operations add records instead of modifying or removing prior records.

## Optional audit exporters

The `AuditExporter` interface mirrors authoritative Badger history to external audit targets:

```go
type AuditExporter interface {
    ExportOperation(OperationRecord) error
    ExportVersion(VersionRecord) error
}
```

`FilesystemExporter` writes readable JSON files under `history/` and `ops/`. `GitExporter` writes the same files in a Git worktree and can commit each operation. `SoftServeExporter` is a Git-compatible wrapper intended for worktrees configured with Soft Serve remotes.

Git and Soft Serve are optional audit mirrors only. BadgerDB remains authoritative, and the database works without Git.
