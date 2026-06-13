# Versioning Layer

Phase 1 adds a layered version-control subsystem in `versioning` that wraps an existing BadgerDB handle. Badger remains the authoritative storage engine; the wrapper writes ordinary Badger keys with reserved prefixes and does not change Badger internals.

## Version records

Every mutation creates an immutable `VersionRecord` containing the logical key, unique version ID, parent version ID, value bytes, value hash, operation ID, operation type, host ID, logical clock, wall-clock timestamp, tombstone flag, and extensible metadata.

The parent pointer records the current version at the time of mutation. This creates a linear version chain for Phase 1 while leaving enough metadata for future branch, timeline, and synchronization features.

## Mutations

`Put` creates a new value version, stores it in the version index and append-only history index, updates the current pointer, and writes an operation record.

`Delete` creates a tombstone version instead of removing history. Historical reads by version ID continue to work after deletion.

`Rollback` reads a historical version and creates a brand-new version whose contents and tombstone state match the selected version. The rollback version points to the previously current version as its parent and records the restored source in metadata, so rollback is itself auditable and never deletes newer history.

## Reads

`Get` resolves `/data/current/{key}` and returns `ErrNotFound` if that current version is a tombstone. `History` scans the append-only history prefix for a key. `GetVersion` loads an immutable version directly by version ID.
