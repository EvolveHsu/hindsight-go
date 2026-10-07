package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
)

// FileStore is the on-disk blob surface behind download_file and the
// attachment read. Upstream backs this with a pluggable object store (Postgres
// or S3); the lite build keeps a local directory, rooted at
// HINDSIGHT_GO_FILE_ROOT (default: <tmp>/hindsight-go-files).
type FileStore struct {
	root string
}

// NewFileStore creates a local file store rooted at dir.
func NewFileStore(dir string) *FileStore { return &FileStore{root: dir} }

// DefaultFileStore builds the store the process uses unless one is injected.
func DefaultFileStore() *FileStore {
	root := strings.TrimSpace(os.Getenv("HINDSIGHT_GO_FILE_ROOT"))
	if root == "" {
		root = filepath.Join(os.TempDir(), "hindsight-go-files")
	}
	return NewFileStore(root)
}

// Root returns the resolved root directory.
func (f *FileStore) Root() string { return f.root }

// safePath resolves key inside the root, rejecting traversal.
func (f *FileStore) safePath(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." || filepath.IsAbs(clean) {
		return "", errors.New("invalid storage key")
	}
	return filepath.Join(f.root, clean), nil
}

// Put writes data at key, creating parent directories.
func (f *FileStore) Put(key string, data []byte) error {
	p, err := f.safePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".hindsight-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, p)
}

// Open returns a reader over a stored blob plus its size. A missing blob is
// reported as fs.ErrNotExist.
func (f *FileStore) Open(key string) (io.ReadSeekCloser, int64, error) {
	p, err := f.safePath(key)
	if err != nil {
		return nil, 0, err
	}
	fh, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	info, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, 0, err
	}
	return fh, info.Size(), nil
}

// ReadAll returns a stored blob.
func (f *FileStore) ReadAll(key string) ([]byte, error) {
	rc, _, err := f.Open(key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// SetFileStore overrides the blob store (tests and non-default deployments).
func (e *Engine) SetFileStore(f *FileStore) { e.files = f }

// fileStore returns the engine's store, falling back to the default root so a
// zero-value Engine still serves exports.
func (e *Engine) fileStore() *FileStore {
	if e.files != nil {
		return e.files
	}
	return DefaultFileStore()
}

// opRecord is one completed operation the lite build remembers so that a
// caller who holds an operation id can read its result. Everything runs
// inline, so records are born completed.
type opRecord struct {
	BankID    string
	Operation string
	Status    string
	CreatedAt time.Time
	Metadata  map[string]any
}

var opRegistry = struct {
	sync.RWMutex
	records map[string]*opRecord
}{records: map[string]*opRecord{}}

// recordOperation stores a completed operation and returns its id.
func recordOperation(bankID, operation string, metadata map[string]any) string {
	id := "op-" + newTransferID()[:16]
	opRegistry.Lock()
	defer opRegistry.Unlock()
	opRegistry.records[id] = &opRecord{
		BankID:    bankID,
		Operation: operation,
		Status:    "completed",
		CreatedAt: time.Now().UTC(),
		Metadata:  metadata,
	}
	return id
}

// lookupOperation returns a recorded operation, if this process ran it.
func lookupOperation(id string) (*opRecord, bool) {
	opRegistry.RLock()
	defer opRegistry.RUnlock()
	rec, ok := opRegistry.records[id]
	return rec, ok
}

// resetOperations clears the registry (tests).
func resetOperations() {
	opRegistry.Lock()
	defer opRegistry.Unlock()
	opRegistry.records = map[string]*opRecord{}
}

// recordedOperations lists a bank's recorded operations, newest first.
func recordedOperations(bankID string) []api.OperationResponse {
	opRegistry.RLock()
	defer opRegistry.RUnlock()
	type entry struct {
		id  string
		rec *opRecord
	}
	entries := make([]entry, 0, len(opRegistry.records))
	for id, rec := range opRegistry.records {
		if bankID != "" && rec.BankID != bankID {
			continue
		}
		entries = append(entries, entry{id: id, rec: rec})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].rec.CreatedAt.After(entries[j].rec.CreatedAt)
	})
	out := make([]api.OperationResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, api.OperationResponse{
			ID:            e.id,
			TaskType:      e.rec.Operation,
			OperationID:   api.OptString{Set: true, Value: e.id},
			OperationType: api.OptString{Set: true, Value: e.rec.Operation},
			Status:        e.rec.Status,
			CreatedAt:     e.rec.CreatedAt.Format(time.RFC3339),
			UpdatedAt:     api.OptString{Set: true, Value: e.rec.CreatedAt.Format(time.RFC3339)},
		})
	}
	return out
}

// BankAttachmentSource is the storage slice behind get_bank_attachment: the
// storage key and media type of one inline attachment, by short id.
type BankAttachmentSource interface {
	BankAttachment(ctx context.Context, bankID, attachmentID string) (storageKey, mediaType string, err error)
}

// GetBankAttachment implements get_bank_attachment
// (GET /banks/{bank_id}/attachments/{attachment_id}).
//
// Upstream serves the bytes from file storage. The lite build reads the same
// row and resolves the key through HINDSIGHT_GO_FILE_ROOT; a deployment whose
// bytes live in S3 needs that root mounted or shared for the read to resolve,
// and answers 404 otherwise — the same response a missing attachment gets.
func (e *Engine) GetBankAttachment(ctx context.Context, params api.GetBankAttachmentParams) (api.GetBankAttachmentRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("Attachment not found")
	}
	src, ok := e.store.(BankAttachmentSource)
	if !ok {
		return nil, notFound("Attachment not found")
	}
	key, _, err := src.BankAttachment(ctx, params.BankID, params.AttachmentID)
	if err != nil {
		return nil, internal(err)
	}
	if key == "" {
		return nil, notFound("Attachment not found")
	}
	reader, _, err := e.fileStore().Open(key)
	if err != nil {
		return nil, notFound("Attachment not found")
	}
	return &api.GetBankAttachmentOK{Data: reader}, nil
}

// DownloadFile implements download_file (GET /default/files/download/{key}).
//
// Only bank-scoped keys resolve: the bank id is parsed out of the key and the
// bank must exist, so the endpoint cannot be used to read another deployment's
// files even if a key leaks.
func (e *Engine) DownloadFile(ctx context.Context, params api.DownloadFileParams) (api.DownloadFileRes, error) {
	bankID, ok := bankIDFromStorageKey(params.Key)
	if !ok {
		return nil, notFound("File not found")
	}
	if b, err := e.store.GetBank(ctx, bankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("File not found")
	}
	reader, _, err := e.fileStore().Open(params.Key)
	if err != nil {
		return nil, notFound("File not found")
	}
	return &api.DownloadFileOK{Data: reader}, nil
}

// bankIDFromStorageKey accepts both upstream layouts:
// tenants/{schema}/banks/{bank_id}/... and banks/{bank_id}/...
func bankIDFromStorageKey(key string) (string, bool) {
	// Archives this process wrote carry the bank id only in the registry, so a
	// flat key can still be authorized against its bank.
	opRegistry.RLock()
	for _, rec := range opRegistry.records {
		stored, _ := rec.Metadata["storage_key"].(string)
		if stored != "" && stored == key {
			bankID := rec.BankID
			opRegistry.RUnlock()
			return bankID, true
		}
	}
	opRegistry.RUnlock()
	parts := strings.Split(key, "/")
	for _, p := range parts {
		if p == ".." || p == "" {
			return "", false
		}
	}
	if len(parts) > 4 && parts[0] == "tenants" && parts[2] == "banks" && parts[3] != "" {
		return parts[3], true
	}
	if len(parts) > 2 && parts[0] == "banks" && parts[1] != "" {
		return parts[1], true
	}
	return "", false
}

// marshalMetadata renders an operation's result_metadata map for the wire.
func marshalMetadata(m map[string]any) api.OptOperationStatusResponseResultMetadata {
	if len(m) == 0 {
		return api.OptOperationStatusResponseResultMetadata{}
	}
	out := api.OperationStatusResponseResultMetadata{}
	for k, v := range m {
		raw, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out[k] = raw
	}
	return api.OptOperationStatusResponseResultMetadata{Set: true, Value: out}
}
