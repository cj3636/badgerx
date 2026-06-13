package versioning

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

const (
	OperationPut      = "put"
	OperationDelete   = "delete"
	OperationRollback = "rollback"
)

var ErrNotFound = badger.ErrKeyNotFound

// VersionRecord is the immutable value-history entry for one logical key.
type VersionRecord struct {
	Key              string            `json:"key"`
	VersionID        string            `json:"version_id"`
	ParentVersionID  string            `json:"parent_version_id,omitempty"`
	Value            []byte            `json:"value,omitempty"`
	ValueHash        string            `json:"value_hash"`
	OperationID      string            `json:"operation_id"`
	Operation        string            `json:"operation"`
	HostID           string            `json:"host_id"`
	LogicalClock     uint64            `json:"logical_clock"`
	WallTimeUnixNano int64             `json:"wall_time_unix_nano"`
	Tombstone        bool              `json:"tombstone"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// OperationRecord is an immutable mutation log entry.
type OperationRecord struct {
	OperationID string `json:"operation_id"`
	Type        string `json:"type"`
	Key         string `json:"key"`
	VersionID   string `json:"version_id"`
	Timestamp   int64  `json:"timestamp"`
	HostID      string `json:"host_id"`
}

// AuditExporter mirrors authoritative Badger history to optional audit targets.
type AuditExporter interface {
	ExportOperation(OperationRecord) error
	ExportVersion(VersionRecord) error
}

// DB is a version-control layer over BadgerDB.
type DB struct {
	db       *badger.DB
	hostID   string
	clock    atomic.Uint64
	exporter AuditExporter
}

type Option func(*DB)

func WithHostID(hostID string) Option          { return func(v *DB) { v.hostID = hostID } }
func WithAuditExporter(e AuditExporter) Option { return func(v *DB) { v.exporter = e } }

func Open(db *badger.DB, opts ...Option) *DB {
	v := &DB{db: db, hostID: defaultHostID()}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

func (v *DB) Put(key string, value []byte) (*VersionRecord, error) {
	return v.mutate(key, value, OperationPut, false, "")
}

func (v *DB) Get(key string) ([]byte, *VersionRecord, error) {
	var rec VersionRecord
	err := v.db.View(func(txn *badger.Txn) error {
		return getJSON(txn, currentKey(key), &rec)
	})
	if err != nil {
		return nil, nil, err
	}
	if rec.Tombstone {
		return nil, &rec, ErrNotFound
	}
	return append([]byte(nil), rec.Value...), &rec, nil
}

func (v *DB) Delete(key string) (*VersionRecord, error) {
	return v.mutate(key, nil, OperationDelete, true, "")
}

func (v *DB) History(key string) ([]VersionRecord, error) {
	prefix := historyPrefix(key)
	out := []VersionRecord{}
	err := v.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			var rec VersionRecord
			if err := item.Value(func(val []byte) error { return json.Unmarshal(val, &rec) }); err != nil {
				return err
			}
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}

func (v *DB) GetVersion(key, versionID string) (*VersionRecord, error) {
	var rec VersionRecord
	err := v.db.View(func(txn *badger.Txn) error { return getJSON(txn, versionKey(key, versionID), &rec) })
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (v *DB) Rollback(key, versionID string) (*VersionRecord, error) {
	target, err := v.GetVersion(key, versionID)
	if err != nil {
		return nil, err
	}
	rec, err := v.mutate(key, target.Value, OperationRollback, target.Tombstone, versionID)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (v *DB) Audit(key string) ([]OperationRecord, error) {
	prefix := opsPrefix(key)
	out := []OperationRecord{}
	err := v.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			var op OperationRecord
			if err := it.Item().Value(func(val []byte) error { return json.Unmarshal(val, &op) }); err != nil {
				return err
			}
			out = append(out, op)
		}
		return nil
	})
	return out, err
}

func (v *DB) mutate(key string, value []byte, op string, tombstone bool, rollbackFrom string) (*VersionRecord, error) {
	if key == "" {
		return nil, errors.New("versioning: key is required")
	}
	now := time.Now().UnixNano()
	clock := v.clock.Add(1)
	vid := fmt.Sprintf("%020d-%s", clock, randomHex(8))
	oid := fmt.Sprintf("%020d-%s", clock, randomHex(8))
	var rec VersionRecord
	var oprec OperationRecord
	err := v.db.Update(func(txn *badger.Txn) error {
		parent := ""
		var current VersionRecord
		if err := getJSON(txn, currentKey(key), &current); err == nil {
			parent = current.VersionID
		} else if err != badger.ErrKeyNotFound {
			return err
		}
		meta := map[string]string{}
		if rollbackFrom != "" {
			meta["rollback_from_version_id"] = rollbackFrom
		}
		rec = VersionRecord{Key: key, VersionID: vid, ParentVersionID: parent, Value: append([]byte(nil), value...), ValueHash: hashValue(value), OperationID: oid, Operation: op, HostID: v.hostID, LogicalClock: clock, WallTimeUnixNano: now, Tombstone: tombstone, Metadata: meta}
		oprec = OperationRecord{OperationID: oid, Type: op, Key: key, VersionID: vid, Timestamp: now, HostID: v.hostID}
		if err := setJSON(txn, versionKey(key, vid), rec); err != nil {
			return err
		}
		if err := setJSON(txn, historyKey(key, now, vid), rec); err != nil {
			return err
		}
		if err := setJSON(txn, currentKey(key), rec); err != nil {
			return err
		}
		if err := setJSON(txn, operationKey(key, now, oid), oprec); err != nil {
			return err
		}
		if tombstone {
			return setJSON(txn, tombstoneKey(key), rec)
		}
		return txn.Delete(tombstoneKey(key))
	})
	if err != nil && err != badger.ErrKeyNotFound {
		return nil, err
	}
	if err == badger.ErrKeyNotFound {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if v.exporter != nil {
		if e := v.exporter.ExportVersion(rec); e != nil {
			return nil, e
		}
		if e := v.exporter.ExportOperation(oprec); e != nil {
			return nil, e
		}
	}
	return &rec, nil
}

func setJSON(txn *badger.Txn, key []byte, val any) error {
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return txn.Set(key, b)
}
func getJSON(txn *badger.Txn, key []byte, dst any) error {
	item, err := txn.Get(key)
	if err != nil {
		return err
	}
	return item.Value(func(v []byte) error { return json.Unmarshal(v, dst) })
}

func currentKey(key string) []byte { return []byte("/data/current/" + key) }
func versionKey(key, versionID string) []byte {
	return []byte("/data/version/" + key + "/" + versionID)
}
func historyPrefix(key string) []byte { return []byte("/data/history/" + key + "/") }
func historyKey(key string, ts int64, versionID string) []byte {
	return []byte(fmt.Sprintf("/data/history/%s/%020d-%s", key, ts, versionID))
}
func tombstoneKey(key string) []byte { return []byte("/data/tombstone/" + key) }
func opsPrefix(key string) []byte    { return []byte("/ops/by-key/" + key + "/") }
func operationKey(key string, ts int64, opID string) []byte {
	return []byte(fmt.Sprintf("/ops/by-key/%s/%020d-%s", key, ts, opID))
}
func hashValue(v []byte) string { h := sha256.Sum256(v); return hex.EncodeToString(h[:]) }
func randomHex(n int) string    { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func defaultHostID() string {
	h, _ := os.Hostname()
	if h == "" {
		return "unknown-host"
	}
	return h
}

// FilesystemExporter writes readable JSON audit files without invoking Git.
type FilesystemExporter struct{ Root string }

func (e FilesystemExporter) ExportOperation(op OperationRecord) error {
	return writeJSON(filepath.Join(e.Root, "ops", safe(op.OperationID)+".json"), op)
}
func (e FilesystemExporter) ExportVersion(rec VersionRecord) error {
	return writeJSON(filepath.Join(e.Root, "history", safe(rec.Key), safe(rec.VersionID)+".json"), rec)
}

// GitExporter mirrors files into a Git worktree and can commit each operation.
type GitExporter struct {
	FilesystemExporter
	Commit bool
}

func (e GitExporter) ExportOperation(op OperationRecord) error {
	if err := e.FilesystemExporter.ExportOperation(op); err != nil {
		return err
	}
	if e.Commit {
		return gitCommit(e.Root, "audit operation "+op.OperationID)
	}
	return nil
}
func (e GitExporter) ExportVersion(rec VersionRecord) error {
	return e.FilesystemExporter.ExportVersion(rec)
}

// SoftServeExporter is a Git-compatible audit mirror intended for worktrees backed by Soft Serve remotes.
type SoftServeExporter struct {
	GitExporter
	Remote string
}

func writeJSON(path string, val any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
func gitCommit(root, msg string) error {
	if err := runGit(root, "add", "."); err != nil {
		return err
	}
	return runGit(root, "commit", "-m", msg)
}
func runGit(root string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
func safe(s string) string {
	s = strings.ReplaceAll(s, string(filepath.Separator), "_")
	s = strings.ReplaceAll(s, "..", "_")
	return s
}

func SortVersions(records []VersionRecord) {
	sort.Slice(records, func(i, j int) bool { return records[i].LogicalClock < records[j].LogicalClock })
}
