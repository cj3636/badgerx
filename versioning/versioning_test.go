package versioning

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/stretchr/testify/require"
)

func newTestDB(t *testing.T) (*DB, func()) {
	t.Helper()
	raw, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	require.NoError(t, err)
	db, err := Open(raw, WithHostID("test-host"))
	require.NoError(t, err)
	return db, func() { require.NoError(t, raw.Close()) }
}

func TestPutUpdateHistoryAndAudit(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	first, err := db.Put("alpha", []byte("one"))
	require.NoError(t, err)
	second, err := db.Put("alpha", []byte("two"))
	require.NoError(t, err)
	val, current, err := db.Get("alpha")
	require.NoError(t, err)
	require.Equal(t, []byte("two"), val)
	require.Equal(t, second.VersionID, current.VersionID)
	require.Equal(t, first.VersionID, second.ParentVersionID)
	hist, err := db.History("alpha")
	require.NoError(t, err)
	require.Len(t, hist, 2)
	old, err := db.GetVersion("alpha", first.VersionID)
	require.NoError(t, err)
	require.Equal(t, []byte("one"), old.Value)
	ops, err := db.Audit("alpha")
	require.NoError(t, err)
	require.Len(t, ops, 2)
	require.Equal(t, first.OperationID, ops[0].OperationID)
}

func TestDeleteCreatesTombstoneAndKeepsHistory(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	first, err := db.Put("alpha", []byte("one"))
	require.NoError(t, err)
	tombstone, err := db.Delete("alpha")
	require.NoError(t, err)
	require.True(t, tombstone.Tombstone)
	require.Equal(t, first.VersionID, tombstone.ParentVersionID)
	_, current, err := db.Get("alpha")
	require.ErrorIs(t, err, ErrNotFound)
	require.True(t, current.Tombstone)
	old, err := db.GetVersion("alpha", first.VersionID)
	require.NoError(t, err)
	require.Equal(t, []byte("one"), old.Value)
	hist, err := db.History("alpha")
	require.NoError(t, err)
	require.Len(t, hist, 2)
}

func TestRollbackCreatesAuditableVersionWithoutDeletingNewerHistory(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	first, err := db.Put("alpha", []byte("one"))
	require.NoError(t, err)
	second, err := db.Put("alpha", []byte("two"))
	require.NoError(t, err)
	rollback, err := db.Rollback("alpha", first.VersionID)
	require.NoError(t, err)
	require.Equal(t, OperationRollback, rollback.Operation)
	require.Equal(t, second.VersionID, rollback.ParentVersionID)
	require.Equal(t, first.VersionID, rollback.Metadata["rollback_from_version_id"])
	val, _, err := db.Get("alpha")
	require.NoError(t, err)
	require.Equal(t, []byte("one"), val)
	hist, err := db.History("alpha")
	require.NoError(t, err)
	require.Len(t, hist, 3)
	stillThere, err := db.GetVersion("alpha", second.VersionID)
	require.NoError(t, err)
	require.Equal(t, []byte("two"), stillThere.Value)
}

func TestFilesystemExporterWritesAuditMirror(t *testing.T) {
	raw, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	require.NoError(t, err)
	defer raw.Close()
	root := t.TempDir()
	db, err := Open(raw, WithAuditExporter(FilesystemExporter{Root: root}))
	require.NoError(t, err)
	rec, err := db.Put("nested/key", []byte("value"))
	require.NoError(t, err)
	versionPath := filepath.Join(root, "history", rec.EncodedKey, safe(rec.VersionID)+".json")
	opPath := filepath.Join(root, "ops", safe(rec.OperationID)+".json")
	require.FileExists(t, versionPath)
	require.FileExists(t, opPath)
	data, err := os.ReadFile(versionPath)
	require.NoError(t, err)
	var exported VersionRecord
	require.NoError(t, json.Unmarshal(data, &exported))
	require.Equal(t, rec.VersionID, exported.VersionID)
}

func TestGitExporterCommitsOperation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	raw, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	require.NoError(t, err)
	defer raw.Close()
	root := t.TempDir()
	require.NoError(t, exec.Command("git", "init", root).Run())
	cmd := exec.Command("git", "config", "user.email", "test@example.com")
	cmd.Dir = root
	require.NoError(t, cmd.Run())
	cmd = exec.Command("git", "config", "user.name", "Test User")
	cmd.Dir = root
	require.NoError(t, cmd.Run())
	db, err := Open(raw, WithAuditExporter(GitExporter{FilesystemExporter: FilesystemExporter{Root: root}, Commit: true}))
	require.NoError(t, err)
	_, err = db.Put("alpha", []byte("one"))
	require.NoError(t, err)
	cmd = exec.Command("git", "rev-list", "--count", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "1\n", string(out))
}

func TestReopenPersistsHostClockHistoryTombstoneOperationsAndExportState(t *testing.T) {
	dir := t.TempDir()
	open := func() (*badger.DB, *DB) {
		raw, err := badger.Open(badger.DefaultOptions(dir).WithLogger(nil))
		require.NoError(t, err)
		db, err := Open(raw)
		require.NoError(t, err)
		return raw, db
	}
	raw, db := open()
	host := db.HostID()
	first, err := db.Put("alpha", []byte("one"))
	require.NoError(t, err)
	_, err = db.Delete("alpha")
	require.NoError(t, err)
	require.NoError(t, db.MarkExportFailed(first.OperationID, os.ErrPermission))
	require.NoError(t, raw.Close())

	raw, db = open()
	defer raw.Close()
	require.Equal(t, host, db.HostID())
	failed, err := db.ListExportFailed(10)
	require.NoError(t, err)
	require.Len(t, failed, 1)
	ops, err := db.ListOperations(OperationListOptions{})
	require.NoError(t, err)
	require.Len(t, ops, 2)
	require.Equal(t, uint64(1), ops[0].LogicalClock)
	require.Equal(t, uint64(2), ops[1].LogicalClock)
	second, err := db.Put("beta", []byte("two"))
	require.NoError(t, err)
	require.Equal(t, uint64(3), second.LogicalClock)
	_, current, err := db.Get("alpha")
	require.ErrorIs(t, err, ErrNotFound)
	require.True(t, current.Tombstone)
	old, err := db.GetVersion("alpha", first.VersionID)
	require.NoError(t, err)
	require.Equal(t, []byte("one"), old.Value)
}

func TestReservedAndWeirdKeys(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	for _, key := range []string{"/meta/x", "/data/x", "/ops/x", "/export/x", "/sync/x", "/snapshots/x", "/timeline/x"} {
		_, err := db.Put(key, []byte("x"))
		require.ErrorIs(t, err, ErrReservedKey)
	}
	keys := []string{"slash/key", "space key", "unicode-☃", "null\x00byte", "control\x01\x02", "looks/meta", string([]byte{0xff, 0x00, 0x7f})}
	value := append([]byte("bin\x00"), bytes.Repeat([]byte{0xab}, 1024)...)
	for _, key := range keys {
		rec, err := db.Put(key, value)
		require.NoError(t, err)
		require.NotContains(t, rec.EncodedKey, "/")
		decoded, err := DecodeKey(rec.EncodedKey)
		require.NoError(t, err)
		require.Equal(t, key, decoded)
		got, _, err := db.Get(key)
		require.NoError(t, err)
		require.Equal(t, value, got)
		hist, err := db.History(key)
		require.NoError(t, err)
		require.Len(t, hist, 1)
		_, err = db.Delete(key)
		require.NoError(t, err)
		_, _, err = db.Get(key)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = db.Rollback(key, rec.VersionID)
		require.NoError(t, err)
	}
}

func TestConcurrentWritesHaveUniqueClocksAndOperationIDs(t *testing.T) {
	db, cleanup := newTestDB(t)
	defer cleanup()
	const n = 50
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) { _, err := db.Put("key", []byte{byte(i)}); errCh <- err }(i)
	}
	for i := 0; i < n; i++ {
		require.NoError(t, <-errCh)
	}
	ops, err := db.ListOperations(OperationListOptions{})
	require.NoError(t, err)
	require.Len(t, ops, n)
	clocks := map[uint64]bool{}
	ids := map[string]bool{}
	for i, op := range ops {
		require.False(t, clocks[op.LogicalClock])
		require.False(t, ids[op.OperationID])
		clocks[op.LogicalClock] = true
		ids[op.OperationID] = true
		if i > 0 {
			require.Greater(t, op.LogicalClock, ops[i-1].LogicalClock)
		}
	}
	hist, err := db.History("key")
	require.NoError(t, err)
	require.Len(t, hist, n)
}

type failingExporter struct{}

func (f failingExporter) ExportOperation(OperationRecord) error { return os.ErrPermission }
func (f failingExporter) ExportVersion(VersionRecord) error     { return nil }

func TestExportFailureRetryAndGitStatusPushFailure(t *testing.T) {
	raw, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	require.NoError(t, err)
	defer raw.Close()
	db, err := Open(raw, WithAuditExporter(failingExporter{}))
	require.NoError(t, err)
	rec, err := db.Put("alpha", []byte("one"))
	require.NoError(t, err)
	failed, err := db.ListExportFailed(10)
	require.NoError(t, err)
	require.Len(t, failed, 1)
	require.Error(t, db.RetryExport(rec.OperationID))
	root := t.TempDir()
	require.NoError(t, exec.Command("git", "init", root).Run())
	ge := GitExporter{FilesystemExporter: FilesystemExporter{Root: root}, Commit: true}
	st, err := ge.GitStatus(len(failed))
	require.NoError(t, err)
	require.True(t, st.Initialized)
	require.Equal(t, 1, st.PendingExports)
	require.ErrorIs(t, ge.GitPush("", ""), ErrRemoteNotConfigured)
}
