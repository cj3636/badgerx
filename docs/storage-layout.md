# Versioning Storage Layout

The versioning layer uses reserved Badger key prefixes so Phase 1 remains cleanly layered over BadgerDB.

```text
/meta/host
/meta/clock/local
/meta/schema/version
/data/current/{encodedKey}
/data/version/{encodedKey}/{versionID}
/data/history/{encodedKey}/{logicalClock}-{versionID}
/data/tombstone/{encodedKey}
/ops/global/{logicalClock}-{operationID}
/ops/by-key/{encodedKey}/{logicalClock}-{operationID}
/ops/by-id/{operationID}
```

## Schema and compatibility

`/meta/schema/version` is set to the Phase 1 schema version. PR #1 data used raw keys and no durable host/clock metadata. This follow-up uses encoded keys for all new writes. Compatibility reads for PR #1 data are intentionally not automatic because raw-key paths can be ambiguous and could collide with internal prefixes. Existing PR #1 databases should be exported or migrated intentionally before using this layout.

## Current pointer

`/data/current/{encodedKey}` stores the current `VersionRecord`. If the current record has `Tombstone=true`, normal `Get` returns not found while preserving tombstone metadata.

## Immutable versions and history

`/data/version/{encodedKey}/{versionID}` stores each immutable version by stable ID. `/data/history/{encodedKey}/{logicalClock}-{versionID}` stores the same record in chronological scan order.

## Tombstones

`/data/tombstone/{encodedKey}` stores the current tombstone record after delete operations. The tombstone is metadata; it does not erase earlier versions.

## Operation log

The global operation log is ordered by logical clock. Per-key and by-ID indexes point to the same serialized operation state, including export status.

## Reserved prefixes

User keys beginning with `/meta/`, `/data/`, `/ops/`, `/export/`, `/sync/`, `/snapshots/`, or `/timeline/` are rejected by the versioning wrapper.
