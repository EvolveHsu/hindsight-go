// Package transfer reads and writes the upstream transfer archive format:
// a ZIP carrying manifest.json plus one documents/NNNNNN.json per document
// (hindsight_api/engine/transfer/schema.py). Documents are stored under a
// zero-padded ordinal rather than their id so arbitrary ids never leak into
// entry names; the real id lives inside each payload.
//
// The lite writer emits the document/fact subset of the format. Imports accept
// archives produced by either implementation as long as they carry
// schema_version 1.
package transfer

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// SchemaVersion is the archive layout this package reads and writes.
const SchemaVersion = 1

// Archive types recorded in the manifest.
const (
	ArchiveDocuments = "documents"
	ArchiveBank      = "bank"
)

// maxArchiveBytes caps the decompressed size of an imported archive.
const maxArchiveBytes = 512 << 20

// Scope records what a producer was asked to carry. Absent on pre-scope
// archives, which readers treat as data+bank_config.
type Scope struct {
	Data       bool `json:"data"`
	BankConfig bool `json:"bank_config"`
	History    bool `json:"history"`
}

// Manifest is the top-level archive descriptor (manifest.json).
type Manifest struct {
	SchemaVersion          int      `json:"schema_version"`
	SourceBankID           string   `json:"source_bank_id"`
	ExportedAt             string   `json:"exported_at,omitempty"`
	DocumentCount          int      `json:"document_count"`
	FactCount              int      `json:"fact_count"`
	ObservationCount       int      `json:"observation_count"`
	MentalModelCount       int      `json:"mental_model_count"`
	KnowledgePageCount     int      `json:"knowledge_page_count"`
	ArchiveType            string   `json:"archive_type"`
	DirectiveCount         int      `json:"directive_count"`
	WebhookCount           int      `json:"webhook_count"`
	IncludesHistory        bool     `json:"includes_history"`
	Scope                  *Scope   `json:"scope,omitempty"`
	AttachmentCount        int      `json:"attachment_count"`
	OperationCount         int      `json:"operation_count"`
	InvalidatedMemoryCount int      `json:"invalidated_memory_count"`
	DocumentIDs            []string `json:"-"`
}

// manifestJSON is the on-the-wire shape. It exists so the Go-only fields stay
// out of the file.
type manifestJSON struct {
	SchemaVersion          int    `json:"schema_version"`
	SourceBankID           string `json:"source_bank_id"`
	ExportedAt             string `json:"exported_at,omitempty"`
	DocumentCount          int    `json:"document_count"`
	FactCount              int    `json:"fact_count"`
	ObservationCount       int    `json:"observation_count"`
	MentalModelCount       int    `json:"mental_model_count"`
	KnowledgePageCount     int    `json:"knowledge_page_count"`
	ArchiveType            string `json:"archive_type"`
	DirectiveCount         int    `json:"directive_count"`
	WebhookCount           int    `json:"webhook_count"`
	IncludesHistory        bool   `json:"includes_history"`
	Scope                  *Scope `json:"scope,omitempty"`
	AttachmentCount        int    `json:"attachment_count"`
	OperationCount         int    `json:"operation_count"`
	InvalidatedMemoryCount int    `json:"invalidated_memory_count"`
}

type documentJSON struct {
	ID           string      `json:"id"`
	OriginalText string      `json:"original_text,omitempty"`
	RetainParams any         `json:"retain_params,omitempty"`
	Tags         []string    `json:"tags,omitempty"`
	CreatedAt    string      `json:"created_at,omitempty"`
	Chunks       []chunkJSON `json:"chunks"`
	Facts        []factJSON  `json:"facts"`
}

type chunkJSON struct {
	ChunkIndex int    `json:"chunk_index"`
	ChunkText  string `json:"chunk_text"`
}

type observationJSON struct {
	SourceID    string   `json:"source_id,omitempty"`
	Text        string   `json:"text"`
	CreatedAt   string   `json:"created_at,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	MentionedAt string   `json:"mentioned_at,omitempty"`
	ProofCount  int      `json:"proof_count,omitempty"`
}

// BankRow is the banks.json entry: the config overrides a restore applies to
// the target bank.
type BankRow struct {
	BankID  string         `json:"bank_id"`
	Name    string         `json:"name,omitempty"`
	Mission string         `json:"mission,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
}

// DirectiveRow is one directives.json entry.
type DirectiveRow struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// MentalModelRow is one mental_models.json entry. Content is deliberately
// absent: a transfer carries how a model is built, never what it currently says.
type MentalModelRow struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	SourceQuery string   `json:"source_query"`
	Tags        []string `json:"tags,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
}

// KnowledgeRow is one data/knowledge_pages.json entry: a folder or a page of
// the bank's knowledge tree.
type KnowledgeRow struct {
	ID            string   `json:"id"`
	ParentID      string   `json:"parent_id,omitempty"`
	Kind          string   `json:"kind"`
	Name          string   `json:"name"`
	MentalModelID string   `json:"mental_model_id,omitempty"`
	SortOrder     int      `json:"sort_order,omitempty"`
	Managed       bool     `json:"managed,omitempty"`
	Description   string   `json:"description,omitempty"`
	Tags          []string `json:"tags,omitempty"`
}

// Sections carries the optional archive members besides documents: the
// whole-bank sections a transfer carries.
type Sections struct {
	Observations   []model.TransferObservation
	Bank           *BankRow
	Directives     []DirectiveRow
	MentalModels   []MentalModelRow
	KnowledgePages []KnowledgeRow
}

type causalJSON struct {
	RelationType    string `json:"relation_type"`
	TargetFactIndex int    `json:"target_fact_index"`
}

type factJSON struct {
	Text            string            `json:"text"`
	SourceID        string            `json:"source_id,omitempty"`
	FactType        string            `json:"fact_type"`
	Context         string            `json:"context,omitempty"`
	EventDate       string            `json:"event_date,omitempty"`
	OccurredStart   string            `json:"occurred_start,omitempty"`
	OccurredEnd     string            `json:"occurred_end,omitempty"`
	MentionedAt     string            `json:"mentioned_at,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Tags            []string          `json:"tags,omitempty"`
	ChunkIndex      *int              `json:"chunk_index,omitempty"`
	Entities        []string          `json:"entities,omitempty"`
	CausalRelations []causalJSON      `json:"causal_relations,omitempty"`
	CreatedAt       string            `json:"created_at,omitempty"`
	ConsolidatedAt  string            `json:"consolidated_at,omitempty"`
}

// Write writes a documents (or whole-bank) archive to w. Observations land in
// the top-level observations.json entry upstream uses.
func Write(w io.Writer, m Manifest, docs []model.TransferDocument, sections Sections) error {
	obs := sections.Observations
	if m.SchemaVersion == 0 {
		m.SchemaVersion = SchemaVersion
	}
	if m.ArchiveType == "" {
		m.ArchiveType = ArchiveDocuments
	}
	if m.ExportedAt == "" {
		m.ExportedAt = time.Now().UTC().Format(time.RFC3339)
	}
	m.DocumentCount = len(docs)
	if m.FactCount == 0 {
		for _, doc := range docs {
			m.FactCount += len(doc.Facts)
		}
	}
	if m.ObservationCount == 0 {
		m.ObservationCount = len(obs)
	}
	if m.DirectiveCount == 0 {
		m.DirectiveCount = len(sections.Directives)
	}
	if m.MentalModelCount == 0 {
		m.MentalModelCount = len(sections.MentalModels)
	}
	if m.KnowledgePageCount == 0 {
		m.KnowledgePageCount = len(sections.KnowledgePages)
	}

	zw := zip.NewWriter(w)
	header, err := zw.Create("manifest.json")
	if err != nil {
		return err
	}
	mj := manifestJSON{
		SchemaVersion:          m.SchemaVersion,
		SourceBankID:           m.SourceBankID,
		ExportedAt:             m.ExportedAt,
		DocumentCount:          m.DocumentCount,
		FactCount:              m.FactCount,
		ObservationCount:       m.ObservationCount,
		MentalModelCount:       m.MentalModelCount,
		KnowledgePageCount:     m.KnowledgePageCount,
		ArchiveType:            m.ArchiveType,
		DirectiveCount:         m.DirectiveCount,
		WebhookCount:           m.WebhookCount,
		IncludesHistory:        m.IncludesHistory,
		Scope:                  m.Scope,
		AttachmentCount:        m.AttachmentCount,
		OperationCount:         m.OperationCount,
		InvalidatedMemoryCount: m.InvalidatedMemoryCount,
	}
	if err := json.NewEncoder(header).Encode(mj); err != nil {
		return err
	}

	for i, doc := range docs {
		name := fmt.Sprintf("documents/%06d.json", i)
		entry, err := zw.Create(name)
		if err != nil {
			return err
		}
		payload := documentJSON{
			ID:           doc.ID,
			OriginalText: doc.OriginalText,
			Tags:         doc.Tags,
			CreatedAt:    doc.CreatedAt,
			Chunks:       make([]chunkJSON, 0, len(doc.Chunks)),
			Facts:        make([]factJSON, 0, len(doc.Facts)),
		}
		for _, c := range doc.Chunks {
			payload.Chunks = append(payload.Chunks, chunkJSON{ChunkIndex: c.ChunkIndex, ChunkText: c.ChunkText})
		}
		for _, f := range doc.Facts {
			fj := factJSON{
				Text:           f.Text,
				SourceID:       f.SourceID,
				FactType:       string(f.FactType),
				Context:        f.Context,
				EventDate:      f.EventDate,
				OccurredStart:  f.OccurredStart,
				OccurredEnd:    f.OccurredEnd,
				MentionedAt:    f.MentionedAt,
				Metadata:       f.Metadata,
				Tags:           f.Tags,
				Entities:       f.Entities,
				CreatedAt:      f.CreatedAt,
				ConsolidatedAt: f.ConsolidatedAt,
			}
			if f.ChunkIndex >= 0 {
				idx := f.ChunkIndex
				fj.ChunkIndex = &idx
			}
			payload.Facts = append(payload.Facts, fj)
		}
		if err := json.NewEncoder(entry).Encode(payload); err != nil {
			return err
		}
	}
	if len(obs) > 0 {
		entry, err := zw.Create("observations.json")
		if err != nil {
			return err
		}
		payload := make([]observationJSON, 0, len(obs))
		for _, o := range obs {
			payload = append(payload, observationJSON{
				SourceID:    o.SourceID,
				Text:        o.Text,
				CreatedAt:   o.CreatedAt,
				Tags:        o.Tags,
				MentionedAt: o.MentionedAt,
				ProofCount:  o.ProofCount,
			})
		}
		if err := json.NewEncoder(entry).Encode(payload); err != nil {
			return err
		}
	}
	if sections.Bank != nil {
		if err := writeJSONEntry(zw, "banks.json", []BankRow{*sections.Bank}); err != nil {
			return err
		}
	}
	if len(sections.Directives) > 0 {
		if err := writeJSONEntry(zw, "directives.json", sections.Directives); err != nil {
			return err
		}
	}
	if len(sections.MentalModels) > 0 {
		if err := writeJSONEntry(zw, "mental_models.json", sections.MentalModels); err != nil {
			return err
		}
	}
	if len(sections.KnowledgePages) > 0 {
		if err := writeJSONEntry(zw, "data/knowledge_pages.json", sections.KnowledgePages); err != nil {
			return err
		}
	}
	return zw.Close()
}

// Read parses an archive. It returns the manifest and the documents it
// carried; unknown entries (blobs/, data/ and the bank-level sections the lite
// build does not restore) are ignored.
func Read(r io.ReaderAt, size int64) (*Manifest, []model.TransferDocument, error) {
	if size > maxArchiveBytes {
		return nil, nil, fmt.Errorf("archive is too large (%d bytes, max %d)", size, int64(maxArchiveBytes))
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, nil, fmt.Errorf("not a valid ZIP archive: %w", err)
	}

	var manifest *Manifest
	var docEntries []*zip.File
	for _, f := range zr.File {
		switch {
		case f.Name == "manifest.json":
			manifest = &Manifest{}
			if err := decodeEntry(f, func(raw []byte) error {
				var mj manifestJSON
				if err := json.Unmarshal(raw, &mj); err != nil {
					return err
				}
				manifest = &Manifest{
					SchemaVersion:          mj.SchemaVersion,
					SourceBankID:           mj.SourceBankID,
					ExportedAt:             mj.ExportedAt,
					DocumentCount:          mj.DocumentCount,
					FactCount:              mj.FactCount,
					ObservationCount:       mj.ObservationCount,
					MentalModelCount:       mj.MentalModelCount,
					KnowledgePageCount:     mj.KnowledgePageCount,
					ArchiveType:            mj.ArchiveType,
					DirectiveCount:         mj.DirectiveCount,
					WebhookCount:           mj.WebhookCount,
					IncludesHistory:        mj.IncludesHistory,
					Scope:                  mj.Scope,
					AttachmentCount:        mj.AttachmentCount,
					OperationCount:         mj.OperationCount,
					InvalidatedMemoryCount: mj.InvalidatedMemoryCount,
				}
				return nil
			}); err != nil {
				return nil, nil, fmt.Errorf("manifest.json: %w", err)
			}
		case strings.HasPrefix(f.Name, "documents/") && strings.HasSuffix(f.Name, ".json"):
			docEntries = append(docEntries, f)
		}
	}
	if manifest == nil {
		return nil, nil, errors.New("archive is missing manifest.json")
	}
	if manifest.SchemaVersion != SchemaVersion {
		return nil, nil, fmt.Errorf("unsupported archive schema version %d (expected %d)", manifest.SchemaVersion, SchemaVersion)
	}
	// Entry order decides document order, not the ZIP's central directory.
	sort.Slice(docEntries, func(i, j int) bool { return docEntries[i].Name < docEntries[j].Name })

	docs := make([]model.TransferDocument, 0, len(docEntries))
	for _, f := range docEntries {
		var payload documentJSON
		if err := decodeEntry(f, func(raw []byte) error { return json.Unmarshal(raw, &payload) }); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path.Clean(f.Name), err)
		}
		if strings.TrimSpace(payload.ID) == "" {
			return nil, nil, fmt.Errorf("%s: document has no id", f.Name)
		}
		doc := model.TransferDocument{
			ID:           payload.ID,
			OriginalText: payload.OriginalText,
			Tags:         payload.Tags,
			CreatedAt:    payload.CreatedAt,
			Chunks:       make([]model.TransferChunk, 0, len(payload.Chunks)),
			Facts:        make([]model.TransferFact, 0, len(payload.Facts)),
		}
		for _, c := range payload.Chunks {
			doc.Chunks = append(doc.Chunks, model.TransferChunk{ChunkIndex: c.ChunkIndex, ChunkText: c.ChunkText})
		}
		for _, f := range payload.Facts {
			fact := model.TransferFact{
				Text:           f.Text,
				SourceID:       f.SourceID,
				FactType:       model.FactType(f.FactType),
				Context:        f.Context,
				EventDate:      f.EventDate,
				OccurredStart:  f.OccurredStart,
				OccurredEnd:    f.OccurredEnd,
				MentionedAt:    f.MentionedAt,
				Metadata:       f.Metadata,
				Tags:           f.Tags,
				ChunkIndex:     -1,
				Entities:       f.Entities,
				CreatedAt:      f.CreatedAt,
				ConsolidatedAt: f.ConsolidatedAt,
			}
			if f.ChunkIndex != nil {
				fact.ChunkIndex = *f.ChunkIndex
			}
			if fact.FactType == "" {
				fact.FactType = model.FactExperience
			}
			doc.Facts = append(doc.Facts, fact)
		}
		docs = append(docs, doc)
	}
	return manifest, docs, nil
}

// ReadObservations returns the observations.json payload, if the archive
// carries one. A missing entry is not an error: document exports omit it.
func ReadObservations(r io.ReaderAt, size int64) ([]model.TransferObservation, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not a valid ZIP archive: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != "observations.json" {
			continue
		}
		var payload []observationJSON
		if err := decodeEntry(f, func(raw []byte) error { return json.Unmarshal(raw, &payload) }); err != nil {
			return nil, fmt.Errorf("observations.json: %w", err)
		}
		out := make([]model.TransferObservation, 0, len(payload))
		for _, o := range payload {
			out = append(out, model.TransferObservation{
				SourceID:    o.SourceID,
				Text:        o.Text,
				CreatedAt:   o.CreatedAt,
				Tags:        o.Tags,
				MentionedAt: o.MentionedAt,
				ProofCount:  o.ProofCount,
			})
		}
		return out, nil
	}
	return nil, nil
}

// ReadSections parses the optional archive members. Missing members yield a
// zero section, never an error: a documents-only export omits them all.
func ReadSections(r io.ReaderAt, size int64) (*Sections, error) {
	if size > maxArchiveBytes {
		return nil, fmt.Errorf("archive is too large (%d bytes, max %d)", size, int64(maxArchiveBytes))
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not a valid ZIP archive: %w", err)
	}
	out := &Sections{}
	for _, f := range zr.File {
		var err error
		switch f.Name {
		case "observations.json":
			var payload []observationJSON
			if err = decodeEntry(f, func(raw []byte) error { return json.Unmarshal(raw, &payload) }); err == nil {
				for _, o := range payload {
					out.Observations = append(out.Observations, model.TransferObservation{
						SourceID:    o.SourceID,
						Text:        o.Text,
						CreatedAt:   o.CreatedAt,
						Tags:        o.Tags,
						MentionedAt: o.MentionedAt,
						ProofCount:  o.ProofCount,
					})
				}
			}
		case "banks.json":
			var payload []BankRow
			if err = decodeEntry(f, func(raw []byte) error { return json.Unmarshal(raw, &payload) }); err == nil && len(payload) > 0 {
				row := payload[0]
				out.Bank = &row
			}
		case "directives.json":
			err = decodeEntry(f, func(raw []byte) error { return json.Unmarshal(raw, &out.Directives) })
		case "mental_models.json":
			err = decodeEntry(f, func(raw []byte) error { return json.Unmarshal(raw, &out.MentalModels) })
		case "data/knowledge_pages.json":
			err = decodeEntry(f, func(raw []byte) error { return json.Unmarshal(raw, &out.KnowledgePages) })
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path.Clean(f.Name), err)
		}
	}
	return out, nil
}

// writeJSONEntry writes one JSON archive member.
func writeJSONEntry(zw *zip.Writer, name string, v any) error {
	entry, err := zw.Create(name)
	if err != nil {
		return err
	}
	return json.NewEncoder(entry).Encode(v)
}

// decodeEntry reads one ZIP member into memory with a size guard.
func decodeEntry(f *zip.File, fn func([]byte) error) error {
	if f.UncompressedSize64 > maxArchiveBytes {
		return fmt.Errorf("entry is too large (%d bytes)", f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(rc, maxArchiveBytes+1)); err != nil {
		return err
	}
	if buf.Len() > maxArchiveBytes {
		return errors.New("entry exceeds the size limit")
	}
	return fn(buf.Bytes())
}
