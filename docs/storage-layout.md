# Versioning Storage Layout

The versioning layer uses reserved Badger key prefixes so Phase 1 remains cleanly layered over BadgerDB.

```text
/data/current/{key}
/data/version/{key}/{versionID}
/data/history/{key}/{timestamp}-{versionID}
/data/tombstone/{key}
/ops/by-key/{key}/{timestamp}-{operationID}
```

## Current pointer

`/data/current/{key}` stores the current `VersionRecord`. If the current record has `Tombstone=true`, normal `Get` returns not found while preserving the tombstone metadata.

## Immutable versions

`/data/version/{key}/{versionID}` stores each immutable `VersionRecord` by stable ID. Historical reads use this index.

## History index

`/data/history/{key}/{timestamp}-{versionID}` stores the same immutable record in chronological scan order for `History(key)`.

## Tombstones

`/data/tombstone/{key}` stores the current tombstone record after delete operations. The tombstone is metadata; it does not erase earlier versions.

## Operation log

`/ops/by-key/{key}/{timestamp}-{operationID}` stores immutable operation records in chronological scan order for `Audit(key)`. This is intentionally local-only in Phase 1 and introduces no synchronization or network behavior.
