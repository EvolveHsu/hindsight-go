package server

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
	"github.com/EvolveHsu/hindsight-go/internal/transfer"
)

// fileRetainMaxBytes caps one uploaded file, matching the upstream batch guard
// in spirit: a lite build keeps the whole file in memory to convert it.
const fileRetainMaxBytes = 64 << 20

// fileRetainRequest is the JSON document carried in the multipart `request`
// field (upstream FileRetainRequest). Unknown keys are ignored, exactly as an
// extra="ignore" pydantic model would.
type fileRetainRequest struct {
	Context       string            `json:"context"`
	Tags          []string          `json:"tags"`
	Timestamp     string            `json:"timestamp"`
	Metadata      map[string]string `json:"metadata"`
	FilesMetadata []fileRetainMeta  `json:"files_metadata"`
}

type fileRetainMeta struct {
	DocumentID string            `json:"document_id"`
	Context    string            `json:"context"`
	Tags       []string          `json:"tags"`
	Timestamp  string            `json:"timestamp"`
	Metadata   map[string]string `json:"metadata"`
}

// FileRetain implements file_retain (POST /banks/{bank_id}/files/retain):
// upload files, convert them to text, and retain the result as documents.
//
// Upstream converts through pluggable parsers (docx/pptx/xlsx/OCR) and stores
// the uploads in object storage. The lite build converts the formats it parses
// natively — text, HTML/XML/JSON and text PDFs — and refuses anything else with
// a 400 naming the supported set, rather than storing a document it cannot read.
func (e *Engine) FileRetain(ctx context.Context, req *api.BodyFileRetainMultipart, params api.FileRetainParams) (api.FileRetainRes, error) {
	if req == nil || len(req.Files) == 0 {
		return nil, badRequest("files must contain at least one file")
	}
	var parsed fileRetainRequest
	if strings.TrimSpace(req.Request) != "" {
		if err := json.Unmarshal([]byte(req.Request), &parsed); err != nil {
			return nil, badRequest("Invalid request JSON: %v", err)
		}
	}
	if len(parsed.FilesMetadata) > 0 && len(parsed.FilesMetadata) != len(req.Files) {
		return nil, badRequest("files_metadata count (%d) must match files count (%d)", len(parsed.FilesMetadata), len(req.Files))
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("Bank '%s' not found", params.BankID)
	}

	items := make([]model.RetainItem, 0, len(req.Files))
	operationIDs := make([]string, 0, len(req.Files))
	for i, f := range req.Files {
		data, err := io.ReadAll(io.LimitReader(f.File, fileRetainMaxBytes+1))
		if err != nil {
			return nil, badRequest("cannot read %q: %v", f.Name, err)
		}
		if len(data) > fileRetainMaxBytes {
			return nil, badRequest("file %q exceeds the %d byte limit for this build", f.Name, fileRetainMaxBytes)
		}
		text, err := transfer.ConvertText(f.Name, f.Header.Get("Content-Type"), data)
		if err != nil {
			return nil, badRequest("%v", err)
		}
		meta := fileRetainMeta{}
		if len(parsed.FilesMetadata) > 0 {
			meta = parsed.FilesMetadata[i]
		}
		docID := meta.DocumentID
		if docID == "" {
			docID = "file_" + newTransferID()
		}
		item := model.RetainItem{
			Content:    text,
			DocumentID: docID,
			Context:    firstNonEmpty(meta.Context, parsed.Context),
			Tags:       append(append([]string{}, parsed.Tags...), meta.Tags...),
			FactType:   model.FactExperience,
		}
		if ts := firstNonEmpty(meta.Timestamp, parsed.Timestamp); ts != "" {
			if t, err := parseRFC3339(ts); err == nil {
				item.MentionedAt = t.UTC().Format(time.RFC3339)
			} else {
				return nil, badRequest("invalid timestamp %q", ts)
			}
		}
		items = append(items, item)
		operationIDs = append(operationIDs, recordOperation(params.BankID, "file_convert_retain", map[string]any{
			"document_id": docID,
			"filename":    f.Name,
			"byte_size":   len(data),
		}))
	}

	if _, err := e.store.Retain(ctx, params.BankID, items); err != nil {
		return nil, mapStoreErr(err)
	}
	return &api.FileRetainResponse{OperationIds: operationIDs}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
