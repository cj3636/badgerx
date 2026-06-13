package versioning

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

const (
	SchemaVersion = "2"

	OperationPut      = "put"
	OperationDelete   = "delete"
	OperationRollback = "rollback"

	ExportPending  = "pending"
	ExportExported = "exported"
	ExportFailed   = "failed"
	ExportSkipped  = "skipped"
)

var (
	ErrNotFound            = badger.ErrKeyNotFound
	ErrReservedKey         = errors.New("versioning: user key uses reserved internal prefix")
	ErrVersionNotFound     = errors.New("versioning: version not found")
	ErrOperationNotFound   = errors.New("versioning: operation not found")
	ErrExportFailed        = errors.New("versioning: export failed")
	ErrGitUnavailable      = errors.New("versioning: git unavailable")
	ErrRemoteNotConfigured = errors.New("versioning: git remote not configured")
	ErrReadOnly            = errors.New("versioning: database opened read-only")
	ErrInvalidHostID       = errors.New("versioning: invalid host id")
	ErrInvalidEncodedKey   = errors.New("versioning: invalid encoded key")
)

var hostIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{2,127}$`)

var ReservedPrefixes = []string{"/meta/", "/data/", "/ops/", "/export/", "/sync/", "/snapshots/", "/timeline/"}

// VersionRecord is the immutable value-history entry for one logical key.
type VersionRecord struct {
	Key              string            `json:"key"`
	EncodedKey       string            `json:"encoded_key"`
	VersionID        string            `json:"version_id"`
	ParentVersionID  string            `json:"parent_version_id,omitempty"`
	Value            []byte            `json:"value,omitempty"`
	ValueHash        string            `json:"value_hash"`
	ValueSize        int               `json:"value_size"`
	OperationID      string            `json:"operation_id"`
	Operation        string            `json:"operation"`
	HostID           string            `json:"host_id"`
	LogicalClock     uint64            `json:"logical_clock"`
	WallTimeUnixNano int64             `json:"wall_time_unix_nano"`
	Tombstone        bool              `json:"tombstone"`
	Priority         bool              `json:"priority"`
	PriorityLevel    int               `json:"priority_level"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// OperationRecord is an immutable mutation log entry with durable export state.
type OperationRecord struct {
	OperationID        string            `json:"operation_id"`
	Type               string            `json:"type"`
	Key                string            `json:"key"`
	EncodedKey         string            `json:"encoded_key"`
	VersionID          string            `json:"version_id"`
	ParentVersionID    string            `json:"parent_version_id,omitempty"`
	HostID             string            `json:"host_id"`
	LogicalClock       uint64            `json:"logical_clock"`
	WallTimeUnixNano   int64             `json:"wall_time_unix_nano"`
	ValueHash          string            `json:"value_hash"`
	ValueSize          int               `json:"value_size"`
	Tombstone          bool              `json:"tombstone"`
	Priority           bool              `json:"priority"`
	PriorityLevel      int               `json:"priority_level"`
	ExportState        string            `json:"export_state"`
	ExportedAtUnixNano int64             `json:"exported_at_unix_nano,omitempty"`
	ExportError        string            `json:"export_error,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

type HostMetadata struct {
	ID            string `json:"id"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	Name          string `json:"name"`
	SchemaVersion string `json:"schema_version"`
}

type OperationListOptions struct {
	Since uint64
	Limit int
}

type GitMirrorStatus struct {
	RepoPath       string `json:"repo_path"`
	Initialized    bool   `json:"initialized"`
	Clean          bool   `json:"clean"`
	Branch         string `json:"branch"`
	HeadCommit     string `json:"head_commit"`
	RemoteName     string `json:"remote_name"`
	RemoteURL      string `json:"remote_url"`
	Ahead          int    `json:"ahead"`
	Behind         int    `json:"behind"`
	PendingExports int    `json:"pending_exports"`
}

type AuditExporter interface {
	ExportOperation(OperationRecord) error
	ExportVersion(VersionRecord) error
}

type DB struct {
	db           *badger.DB
	hostID       string
	exporter     AuditExporter
	strictExport bool
	readOnly     bool
	mu           sync.Mutex
}
type Option func(*DB)

func WithHostID(hostID string) Option          { return func(v *DB) { v.hostID = hostID } }
func WithAuditExporter(e AuditExporter) Option { return func(v *DB) { v.exporter = e } }
func WithStrictExport(strict bool) Option      { return func(v *DB) { v.strictExport = strict } }
func WithReadOnly(readOnly bool) Option        { return func(v *DB) { v.readOnly = readOnly } }

func Open(db *badger.DB, opts ...Option) (*DB, error) {
	v := &DB{db: db}
	for _, opt := range opts {
		opt(v)
	}
	if err := v.initMetadata(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *DB) HostID() string { return v.hostID }
func (v *DB) Put(key string, value []byte) (*VersionRecord, error) {
	return v.mutate(key, value, OperationPut, false, "")
}
func (v *DB) Delete(key string) (*VersionRecord, error) {
	return v.mutate(key, nil, OperationDelete, true, "")
}

func (v *DB) Get(key string) ([]byte, *VersionRecord, error) {
	if err := validateUserKey(key); err != nil {
		return nil, nil, err
	}
	var rec VersionRecord
	err := v.db.View(func(txn *badger.Txn) error { return getJSON(txn, currentKey(EncodeKey(key)), &rec) })
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if rec.Tombstone {
		return nil, &rec, ErrNotFound
	}
	return append([]byte(nil), rec.Value...), &rec, nil
}
func (v *DB) History(key string) ([]VersionRecord, error) {
	if err := validateUserKey(key); err != nil {
		return nil, err
	}
	out := []VersionRecord{}
	prefix := historyPrefix(EncodeKey(key))
	err := v.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			var rec VersionRecord
			if err := it.Item().Value(func(val []byte) error { return json.Unmarshal(val, &rec) }); err != nil {
				return err
			}
			out = append(out, rec)
		}
		return nil
	})
	return out, err
}
func (v *DB) GetVersion(key, versionID string) (*VersionRecord, error) {
	if err := validateUserKey(key); err != nil {
		return nil, err
	}
	var rec VersionRecord
	err := v.db.View(func(txn *badger.Txn) error { return getJSON(txn, versionKey(EncodeKey(key), versionID), &rec) })
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrVersionNotFound
	}
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
	return v.mutate(key, target.Value, OperationRollback, target.Tombstone, versionID)
}
func (v *DB) Audit(key string) ([]OperationRecord, error) {
	if err := validateUserKey(key); err != nil {
		return nil, err
	}
	return v.listOps(opsByKeyPrefix(EncodeKey(key)), OperationListOptions{})
}
func (v *DB) ListOperations(opts OperationListOptions) ([]OperationRecord, error) {
	return v.listOps(globalOpsPrefix(), opts)
}
func (v *DB) ListOperationsSince(clock uint64, limit int) ([]OperationRecord, error) {
	return v.ListOperations(OperationListOptions{Since: clock, Limit: limit})
}
func (v *DB) GetOperation(operationID string) (*OperationRecord, error) {
	var op OperationRecord
	err := v.db.View(func(txn *badger.Txn) error { return getJSON(txn, opByIDKey(operationID), &op) })
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrOperationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &op, nil
}

func (v *DB) mutate(key string, value []byte, op string, tombstone bool, rollbackFrom string) (*VersionRecord, error) {
	if v.readOnly {
		return nil, ErrReadOnly
	}
	if err := validateUserKey(key); err != nil {
		return nil, err
	}
	v.mu.Lock()
	encoded := EncodeKey(key)
	now := time.Now().UnixNano()
	var rec VersionRecord
	var oprec OperationRecord
	err := v.db.Update(func(txn *badger.Txn) error {
		clock, err := nextClock(txn)
		if err != nil {
			return err
		}
		parent := ""
		var current VersionRecord
		if err := getJSON(txn, currentKey(encoded), &current); err == nil {
			parent = current.VersionID
		} else if !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		meta := map[string]string{}
		if rollbackFrom != "" {
			meta["rollback_from_version_id"] = rollbackFrom
		}
		vid := fmt.Sprintf("%s:%020d:%s", v.hostID, clock, randomHex(8))
		oid := fmt.Sprintf("%s:%020d:%s", v.hostID, clock, randomHex(16))
		hash := hashValue(value)
		rec = VersionRecord{Key: key, EncodedKey: encoded, VersionID: vid, ParentVersionID: parent, Value: append([]byte(nil), value...), ValueHash: hash, ValueSize: len(value), OperationID: oid, Operation: op, HostID: v.hostID, LogicalClock: clock, WallTimeUnixNano: now, Tombstone: tombstone, Metadata: meta}
		oprec = OperationRecord{OperationID: oid, Type: op, Key: key, EncodedKey: encoded, VersionID: vid, ParentVersionID: parent, HostID: v.hostID, LogicalClock: clock, WallTimeUnixNano: now, ValueHash: hash, ValueSize: len(value), Tombstone: tombstone, ExportState: initialExportState(v.exporter), Metadata: copyMap(meta)}
		for _, p := range []struct {
			k   []byte
			val any
		}{{versionKey(encoded, vid), rec}, {historyKey(encoded, clock, vid), rec}, {currentKey(encoded), rec}, {globalOpKey(clock, oid), oprec}, {opByKeyKey(encoded, clock, oid), oprec}, {opByIDKey(oid), oprec}} {
			if err := setJSON(txn, p.k, p.val); err != nil {
				return err
			}
		}
		if tombstone {
			return setJSON(txn, tombstoneKey(encoded), rec)
		}
		if err := txn.Delete(tombstoneKey(encoded)); err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		return nil
	})
	v.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if v.exporter == nil {
		return &rec, nil
	}
	if err := v.exporter.ExportVersion(rec); err != nil {
		_ = v.MarkExportFailed(oprec.OperationID, err)
		if v.strictExport {
			return nil, errors.Join(ErrExportFailed, err)
		}
		return &rec, nil
	}
	if err := v.exporter.ExportOperation(oprec); err != nil {
		_ = v.MarkExportFailed(oprec.OperationID, err)
		if v.strictExport {
			return nil, errors.Join(ErrExportFailed, err)
		}
		return &rec, nil
	}
	_ = v.MarkExported(oprec.OperationID, nil)
	return &rec, nil
}

func (v *DB) listOps(prefix []byte, opts OperationListOptions) ([]OperationRecord, error) {
	out := []OperationRecord{}
	err := v.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			var op OperationRecord
			if err := it.Item().Value(func(val []byte) error { return json.Unmarshal(val, &op) }); err != nil {
				return err
			}
			if op.LogicalClock <= opts.Since {
				continue
			}
			out = append(out, op)
			if opts.Limit > 0 && len(out) >= opts.Limit {
				break
			}
		}
		return nil
	})
	return out, err
}

func (v *DB) ListExportPending(limit int) ([]OperationRecord, error) {
	return v.listExportState(ExportPending, limit)
}
func (v *DB) ListExportFailed(limit int) ([]OperationRecord, error) {
	return v.listExportState(ExportFailed, limit)
}
func (v *DB) listExportState(state string, limit int) ([]OperationRecord, error) {
	ops, err := v.ListOperations(OperationListOptions{})
	if err != nil {
		return nil, err
	}
	out := []OperationRecord{}
	for _, op := range ops {
		if op.ExportState == state {
			out = append(out, op)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (v *DB) RetryExport(operationID string) error {
	if v.exporter == nil {
		return v.MarkExported(operationID, map[string]string{"export": "skipped_no_exporter"})
	}
	op, err := v.GetOperation(operationID)
	if err != nil {
		return err
	}
	rec, err := v.GetVersion(op.Key, op.VersionID)
	if err != nil {
		return err
	}
	if err := v.exporter.ExportVersion(*rec); err != nil {
		_ = v.MarkExportFailed(operationID, err)
		return errors.Join(ErrExportFailed, err)
	}
	if err := v.exporter.ExportOperation(*op); err != nil {
		_ = v.MarkExportFailed(operationID, err)
		return errors.Join(ErrExportFailed, err)
	}
	return v.MarkExported(operationID, nil)
}
func (v *DB) MarkExported(operationID string, metadata map[string]string) error {
	return v.updateOperation(operationID, func(op *OperationRecord) {
		op.ExportState = ExportExported
		op.ExportedAtUnixNano = time.Now().UnixNano()
		op.ExportError = ""
		merge(op.Metadata, metadata)
	})
}
func (v *DB) MarkExportFailed(operationID string, err error) error {
	return v.updateOperation(operationID, func(op *OperationRecord) { op.ExportState = ExportFailed; op.ExportError = sanitizeError(err) })
}
func (v *DB) updateOperation(operationID string, fn func(*OperationRecord)) error {
	if v.readOnly {
		return ErrReadOnly
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.db.Update(func(txn *badger.Txn) error {
		var op OperationRecord
		if err := getJSON(txn, opByIDKey(operationID), &op); err != nil {
			if errors.Is(err, badger.ErrKeyNotFound) {
				return ErrOperationNotFound
			}
			return err
		}
		fn(&op)
		return writeOpIndexes(txn, op)
	})
}
func writeOpIndexes(txn *badger.Txn, op OperationRecord) error {
	for _, k := range [][]byte{opByIDKey(op.OperationID), globalOpKey(op.LogicalClock, op.OperationID), opByKeyKey(op.EncodedKey, op.LogicalClock, op.OperationID)} {
		if err := setJSON(txn, k, op); err != nil {
			return err
		}
	}
	return nil
}

func (v *DB) initMetadata() error {
	if v.readOnly {
		return v.db.View(func(txn *badger.Txn) error {
			var meta HostMetadata
			if err := getJSON(txn, metaHostKey(), &meta); err != nil {
				return err
			}
			if v.hostID != "" && v.hostID != meta.ID {
				return fmt.Errorf("%w: stored host id %q differs from requested %q", ErrInvalidHostID, meta.ID, v.hostID)
			}
			v.hostID = meta.ID
			if !ValidHostID(v.hostID) {
				return ErrInvalidHostID
			}
			_, err := readClock(txn)
			return err
		})
	}
	now := time.Now().UnixNano()
	return v.db.Update(func(txn *badger.Txn) error {
		var meta HostMetadata
		err := getJSON(txn, metaHostKey(), &meta)
		if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}
		if v.hostID != "" {
			if !ValidHostID(v.hostID) {
				return ErrInvalidHostID
			}
			if meta.ID != "" && meta.ID != v.hostID {
				return fmt.Errorf("%w: stored host id %q differs from requested %q", ErrInvalidHostID, meta.ID, v.hostID)
			}
			meta.ID = v.hostID
		} else if meta.ID != "" {
			v.hostID = meta.ID
		} else {
			v.hostID = "host-" + randomHex(16)
			meta.ID = v.hostID
			meta.CreatedAt = now
			meta.Name = defaultHostName()
		}
		if !ValidHostID(v.hostID) {
			return ErrInvalidHostID
		}
		if meta.CreatedAt == 0 {
			meta.CreatedAt = now
		}
		meta.UpdatedAt = now
		meta.SchemaVersion = SchemaVersion
		if err := setJSON(txn, metaHostKey(), meta); err != nil {
			return err
		}
		if err := txn.Set(schemaVersionKey(), []byte(SchemaVersion)); err != nil {
			return err
		}
		_, err = readClock(txn)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return setClock(txn, 0)
		}
		return err
	})
}
func nextClock(txn *badger.Txn) (uint64, error) {
	c, err := readClock(txn)
	if errors.Is(err, badger.ErrKeyNotFound) {
		c = 0
	} else if err != nil {
		return 0, err
	}
	c++
	return c, setClock(txn, c)
}
func readClock(txn *badger.Txn) (uint64, error) {
	item, err := txn.Get(clockKey())
	if err != nil {
		return 0, err
	}
	var c uint64
	err = item.Value(func(v []byte) error { n, e := strconv.ParseUint(string(v), 10, 64); c = n; return e })
	return c, err
}
func setClock(txn *badger.Txn, c uint64) error {
	return txn.Set(clockKey(), []byte(strconv.FormatUint(c, 10)))
}

func EncodeKey(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }
func DecodeKey(encoded string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrInvalidEncodedKey
	}
	return string(b), nil
}
func validateUserKey(key string) error {
	for _, p := range ReservedPrefixes {
		if strings.HasPrefix(key, p) {
			return fmt.Errorf("%w: %q", ErrReservedKey, p)
		}
	}
	return nil
}
func ValidHostID(id string) bool { return hostIDPattern.MatchString(id) }
func initialExportState(e AuditExporter) string {
	if e == nil {
		return ExportSkipped
	}
	return ExportPending
}
func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}
func copyMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func merge(dst, src map[string]string) {
	if dst == nil || src == nil {
		return
	}
	for k, v := range src {
		dst[k] = v
	}
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
func currentKey(e string) []byte      { return []byte("/data/current/" + e) }
func versionKey(e, vid string) []byte { return []byte("/data/version/" + e + "/" + vid) }
func historyPrefix(e string) []byte   { return []byte("/data/history/" + e + "/") }
func historyKey(e string, c uint64, vid string) []byte {
	return []byte(fmt.Sprintf("/data/history/%s/%020d-%s", e, c, vid))
}
func tombstoneKey(e string) []byte { return []byte("/data/tombstone/" + e) }
func globalOpsPrefix() []byte      { return []byte("/ops/global/") }
func globalOpKey(c uint64, oid string) []byte {
	return []byte(fmt.Sprintf("/ops/global/%020d-%s", c, oid))
}
func opsByKeyPrefix(e string) []byte { return []byte("/ops/by-key/" + e + "/") }
func opByKeyKey(e string, c uint64, oid string) []byte {
	return []byte(fmt.Sprintf("/ops/by-key/%s/%020d-%s", e, c, oid))
}
func opByIDKey(oid string) []byte { return []byte("/ops/by-id/" + oid) }
func metaHostKey() []byte         { return []byte("/meta/host") }
func clockKey() []byte            { return []byte("/meta/clock/local") }
func schemaVersionKey() []byte    { return []byte("/meta/schema/version") }
func hashValue(v []byte) string   { h := sha256.Sum256(v); return hex.EncodeToString(h[:]) }
func randomHex(n int) string      { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func defaultHostName() string {
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
	return writeJSON(filepath.Join(e.Root, "history", rec.EncodedKey, safe(rec.VersionID)+".json"), rec)
}

type GitExporter struct {
	FilesystemExporter
	Commit  bool
	GitPath string
}

func (e GitExporter) ExportOperation(op OperationRecord) error {
	if err := e.FilesystemExporter.ExportOperation(op); err != nil {
		return err
	}
	if e.Commit {
		return gitCommit(e.gitPath(), e.Root, "audit operation "+op.OperationID)
	}
	return nil
}
func (e GitExporter) ExportVersion(rec VersionRecord) error {
	return e.FilesystemExporter.ExportVersion(rec)
}
func (e GitExporter) gitPath() string {
	if e.GitPath != "" {
		return e.GitPath
	}
	return "git"
}
func (e GitExporter) GitStatus(pending int) (*GitMirrorStatus, error) {
	return gitStatus(e.gitPath(), e.Root, pending)
}
func (e GitExporter) GitPush(remote, branch string) error {
	return gitPush(e.gitPath(), e.Root, remote, branch)
}

type SoftServeExporter struct {
	GitExporter
	Remote string
}

func (e SoftServeExporter) ValidateRemote() error { return ValidateSoftServeRemote(e.Remote) }
func (e SoftServeExporter) ConfigureRemote(name string) error {
	if err := e.ValidateRemote(); err != nil {
		return err
	}
	if name == "" {
		name = "origin"
	}
	_ = runGit(e.gitPath(), e.Root, "remote", "remove", name)
	return runGit(e.gitPath(), e.Root, "remote", "add", name, e.Remote)
}
func (e SoftServeExporter) TestRemote() error {
	if err := e.ValidateRemote(); err != nil {
		return err
	}
	return runGit(e.gitPath(), e.Root, "ls-remote", e.Remote)
}
func ValidateSoftServeRemote(remote string) error {
	if strings.HasPrefix(remote, "ssh://") {
		u, err := url.Parse(remote)
		if err == nil && u.Host != "" && u.User != nil {
			return nil
		}
	}
	if strings.Contains(remote, "@") && strings.Contains(remote, ":") {
		return nil
	}
	return fmt.Errorf("invalid Soft Serve SSH remote %q", remote)
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
func gitCommit(git, root, msg string) error {
	if err := runGit(git, root, "add", "."); err != nil {
		return err
	}
	return runGit(git, root, "commit", "-m", msg)
}
func runGit(git, root string, args ...string) error {
	if _, err := exec.LookPath(git); err != nil {
		return ErrGitUnavailable
	}
	cmd := exec.Command(git, args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, redact(string(out)))
	}
	return nil
}
func gitOutput(git, root string, args ...string) (string, error) {
	if _, err := exec.LookPath(git); err != nil {
		return "", ErrGitUnavailable
	}
	cmd := exec.Command(git, args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, redact(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
func gitStatus(git, root string, pending int) (*GitMirrorStatus, error) {
	st := &GitMirrorStatus{RepoPath: root, PendingExports: pending}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return st, nil
	}
	st.Initialized = true
	out, err := gitOutput(git, root, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	st.Clean = out == ""
	st.Branch, _ = gitOutput(git, root, "branch", "--show-current")
	st.HeadCommit, _ = gitOutput(git, root, "rev-parse", "--verify", "HEAD")
	remotes, _ := gitOutput(git, root, "remote")
	if remotes != "" {
		st.RemoteName = strings.Fields(remotes)[0]
		st.RemoteURL, _ = gitOutput(git, root, "remote", "get-url", st.RemoteName)
		st.RemoteURL = redact(st.RemoteURL)
	}
	return st, nil
}
func gitPush(git, root, remote, branch string) error {
	if remote == "" {
		rs, _ := gitOutput(git, root, "remote")
		fs := strings.Fields(rs)
		if len(fs) == 0 {
			return ErrRemoteNotConfigured
		}
		remote = fs[0]
	}
	if branch == "" {
		branch, _ = gitOutput(git, root, "branch", "--show-current")
	}
	if branch == "" {
		branch = "HEAD"
	}
	return runGit(git, root, "push", remote, branch)
}
func redact(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		prefix := s[:i+3]
		rest := s[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			return prefix + "<redacted>@" + rest[at+1:]
		}
	}
	return strings.TrimSpace(s)
}
func safe(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "_", ":", "_")
	return r.Replace(s)
}
func SortVersions(records []VersionRecord) {
	sort.Slice(records, func(i, j int) bool { return records[i].LogicalClock < records[j].LogicalClock })
}
func EqualKeys(a, b string) bool { return bytes.Equal([]byte(a), []byte(b)) }
