# Audit Log

Every versioned mutation writes an immutable `OperationRecord`. Operation records are the Phase 1 audit trail and are intended to support future synchronization work, but this phase does not implement networking, replication, consensus, or conflict resolution.

## Operation records

Operation records contain operation ID, type, original key, encoded key, resulting version ID, parent version ID, host ID, logical clock, wall-clock timestamp, value hash and size, tombstone state, priority metadata fields, export status, and extensible metadata.

## Global and per-key logs

Each mutation writes three operation indexes:

```text
/ops/global/{logicalClock}-{operationID}
/ops/by-key/{encodedKey}/{logicalClock}-{operationID}
/ops/by-id/{operationID}
```

`ListOperations` scans the global ordered stream. `ListOperationsSince` resumes from a logical clock checkpoint. `GetOperation` retrieves a stable operation by ID. `Audit(key)` remains a per-key operation scan.

## Export states

Operation export state is durable and indexed under `/export/state/{state}/` for efficient pending/failed/exported/skipped scans. State may be `pending`, `exported`, `failed`, or `skipped`. Database mutations succeed independently of export by default. Strict export mode can opt into returning export failures from mutations.

APIs include `ListExportPending`, `ListExportFailed`, `RetryExport`, `MarkExported`, and `MarkExportFailed`. Export errors are truncated before storage to reduce accidental leakage.

## Optional audit exporters

The `AuditExporter` interface mirrors authoritative Badger history to external audit targets. `FilesystemExporter` writes readable JSON files under `history/` and `ops/`. `GitExporter` writes the same files in a Git worktree and can commit each operation. `SoftServeExporter` treats Soft Serve as a normal SSH Git remote.

Git and Soft Serve are audit mirrors only. They are not database replication or synchronization.
