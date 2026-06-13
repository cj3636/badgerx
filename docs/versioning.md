# Versioning Layer

Phase 1 adds a local, single-node version-control subsystem in `versioning` that wraps an existing BadgerDB handle. Badger remains the authoritative storage engine; the wrapper writes ordinary Badger keys with reserved prefixes and does not change Badger internals.

## Durable host identity

On first open, the wrapper generates a durable host ID and stores host metadata under `/meta/host`. Subsequent opens reuse the same ID. Callers may intentionally set a host ID with `WithHostID`, but the value is validated and must match an already-initialized database; the wrapper does not silently change host identity.

## Durable logical clock

The local logical clock is stored at `/meta/clock/local`. Each mutation increments the clock inside the same Badger transaction as the version and operation records. The clock survives close/reopen and provides stable ordering for the global operation log.

## Version records

Every mutation creates an immutable `VersionRecord` containing the original key, encoded key, version ID, parent version ID, value, value hash and size, operation ID, operation type, host ID, logical clock, wall-clock time, tombstone flag, priority metadata fields, and extensible metadata.

Version IDs and operation IDs include host ID, logical clock, and random bytes. This makes them globally unique in practice while remaining traceable to local host and clock metadata.

## Mutations

`Put` creates a new value version, stores it in the version index and append-only history index, updates the current pointer, and writes global/per-key/by-ID operation records.

`Delete` creates a tombstone version instead of removing history. Historical reads by version ID continue to work after deletion.

`Rollback` reads a historical version and creates a brand-new version whose contents and tombstone state match the selected version. The rollback version points to the previously current version as its parent and records the restored source in metadata, so rollback is itself auditable and never deletes newer history.

## Key safety

User keys are never embedded raw in internal Badger keys. The wrapper uses deterministic `base64.RawURLEncoding` for internal paths and stores the original key in records for user-facing APIs. Keys containing slashes, null bytes, control characters, Unicode, and arbitrary binary bytes are supported.

## Reserved user-key prefixes

The wrapper rejects user keys beginning with reserved internal prefixes: `/meta/`, `/data/`, `/ops/`, `/export/`, `/sync/`, `/snapshots/`, and `/timeline/`. Some are reserved for later phases, but no sync, timeline, or snapshot behavior is implemented in Phase 1.
