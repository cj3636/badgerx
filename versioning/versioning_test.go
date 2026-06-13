package versioning

import (
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
	return Open(raw, WithHostID("test-host")), func() { require.NoError(t, raw.Close()) }
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
	db := Open(raw, WithAuditExporter(FilesystemExporter{Root: root}))
	rec, err := db.Put("nested/key", []byte("value"))
	require.NoError(t, err)
	versionPath := filepath.Join(root, "history", "nested_key", rec.VersionID+".json")
	opPath := filepath.Join(root, "ops", rec.OperationID+".json")
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
	db := Open(raw, WithAuditExporter(GitExporter{FilesystemExporter: FilesystemExporter{Root: root}, Commit: true}))
	_, err = db.Put("alpha", []byte("one"))
	require.NoError(t, err)
	cmd = exec.Command("git", "rev-list", "--count", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "1\n", string(out))
}
