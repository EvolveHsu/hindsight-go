package transfer

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/json"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// TestArchiveRoundTrip pins the archive layout: manifest, documents and the
// whole-bank sections survive a write/read cycle.
func TestArchiveRoundTrip(t *testing.T) {
	docs := []model.TransferDocument{{
		ID:           "notes",
		OriginalText: "Ada prefers tea over coffee",
		Tags:         []string{"person:ada"},
		Chunks:       []model.TransferChunk{{ChunkIndex: 0, ChunkText: "Ada prefers tea"}},
		Facts: []model.TransferFact{{
			Text:       "Ada prefers tea over coffee",
			FactType:   model.FactExperience,
			Tags:       []string{"person:ada"},
			Entities:   []string{"Ada"},
			ChunkIndex: 0,
		}},
	}}
	sections := Sections{
		Observations:   []model.TransferObservation{{Text: "Ada drinks tea", Tags: []string{"person:ada"}, ProofCount: 2}},
		Bank:           &BankRow{BankID: "alpha", Config: map[string]any{"reflect_mission": "be brief"}},
		Directives:     []DirectiveRow{{Name: "tone", Content: "be warm"}},
		MentalModels:   []MentalModelRow{{ID: "mm-1", Name: "Profile", SourceQuery: "who is Ada?"}},
		KnowledgePages: []KnowledgeRow{{ID: "kp-1", Kind: "page", Name: "Ada", MentalModelID: "mm-1"}},
	}

	var buf bytes.Buffer
	if err := Write(&buf, Manifest{SourceBankID: "alpha", ArchiveType: ArchiveBank}, docs, sections); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("PK")) {
		t.Fatalf("archive is not a ZIP: %q", buf.Bytes()[:2])
	}

	manifest, gotDocs, err := Read(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if manifest.SourceBankID != "alpha" || manifest.ArchiveType != ArchiveBank {
		t.Errorf("manifest = %+v", manifest)
	}
	if manifest.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", manifest.SchemaVersion, SchemaVersion)
	}
	if manifest.DocumentCount != 1 || manifest.FactCount != 1 || manifest.ObservationCount != 1 ||
		manifest.DirectiveCount != 1 || manifest.MentalModelCount != 1 || manifest.KnowledgePageCount != 1 {
		t.Errorf("manifest counts = %+v", manifest)
	}
	if len(gotDocs) != 1 || gotDocs[0].ID != "notes" || len(gotDocs[0].Facts) != 1 {
		t.Fatalf("documents = %+v", gotDocs)
	}
	if gotDocs[0].Facts[0].Entities[0] != "Ada" || gotDocs[0].Facts[0].ChunkIndex != 0 {
		t.Errorf("fact = %+v", gotDocs[0].Facts[0])
	}

	gotSections, err := ReadSections(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("ReadSections: %v", err)
	}
	if len(gotSections.Observations) != 1 || gotSections.Observations[0].Text != "Ada drinks tea" {
		t.Errorf("observations = %+v", gotSections.Observations)
	}
	if gotSections.Bank == nil || gotSections.Bank.Config["reflect_mission"] != "be brief" {
		t.Errorf("bank row = %+v", gotSections.Bank)
	}
	if len(gotSections.Directives) != 1 || gotSections.Directives[0].Name != "tone" {
		t.Errorf("directives = %+v", gotSections.Directives)
	}
	if len(gotSections.MentalModels) != 1 || gotSections.MentalModels[0].ID != "mm-1" {
		t.Errorf("mental models = %+v", gotSections.MentalModels)
	}
	if len(gotSections.KnowledgePages) != 1 || gotSections.KnowledgePages[0].ID != "kp-1" {
		t.Errorf("knowledge pages = %+v", gotSections.KnowledgePages)
	}
}

// TestArchiveDocumentsOnlyOmitsSections keeps the document export small: a
// documents-only archive carries no bank-level entries and reads back empty
// sections rather than erroring.
func TestArchiveDocumentsOnlyOmitsSections(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Manifest{SourceBankID: "alpha"}, nil, Sections{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sections, err := ReadSections(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("ReadSections: %v", err)
	}
	if sections.Bank != nil || len(sections.Directives) != 0 || len(sections.MentalModels) != 0 || len(sections.KnowledgePages) != 0 {
		t.Errorf("sections = %+v, want empty", sections)
	}
}

// TestArchiveRejectsUnknownSchema guards the version gate: a future archive
// layout must be refused rather than half-imported.
func TestArchiveRejectsUnknownSchema(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Manifest{SchemaVersion: SchemaVersion + 1, SourceBankID: "alpha"}, nil, Sections{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, _, err := Read(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err == nil {
		t.Fatal("Read accepted an unsupported schema version")
	}
}

// TestArchiveReadsUpstreamShape checks interop with the Python writer: the
// manifest and document entries it emits (extra keys and all) parse.
func TestArchiveReadsUpstreamShape(t *testing.T) {
	manifest := map[string]any{
		"schema_version": 1,
		"source_bank_id": "upstream",
		"document_count": 1,
		"fact_count":     1,
		"archive_type":   "documents",
		"scope":          map[string]any{"data": true, "bank_config": false, "history": false},
	}
	doc := map[string]any{
		"id":            "doc-1",
		"original_text": "the sky is blue",
		"tags":          []string{"topic:sky"},
		"chunks":        []map[string]any{{"chunk_index": 0, "chunk_text": "the sky is blue"}},
		"facts": []map[string]any{{
			"text":             "the sky is blue",
			"source_id":        "unit-1",
			"fact_type":        "world",
			"metadata":         map[string]string{"source": "note"},
			"tags":             []string{"topic:sky"},
			"chunk_index":      0,
			"entities":         []string{"sky"},
			"causal_relations": []map[string]any{},
			"created_at":       "2026-01-01T00:00:00Z",
		}},
	}
	raw := buildArchive(t, map[string]any{"manifest.json": manifest, "documents/000000.json": doc})

	gotManifest, docs, err := Read(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if gotManifest.SourceBankID != "upstream" || gotManifest.Scope == nil || !gotManifest.Scope.Data {
		t.Errorf("manifest = %+v", gotManifest)
	}
	if len(docs) != 1 || docs[0].Facts[0].FactType != model.FactWorld {
		t.Fatalf("docs = %+v", docs)
	}
	if docs[0].Facts[0].Metadata["source"] != "note" {
		t.Errorf("metadata = %v", docs[0].Facts[0].Metadata)
	}
}

// TestConvertTextHandlesTextFormats covers the plain-text path of file_retain.
func TestConvertTextHandlesTextFormats(t *testing.T) {
	text, err := ConvertText("notes.md", "text/markdown", []byte("hello\nworld\n"))
	if err != nil {
		t.Fatalf("ConvertText: %v", err)
	}
	if text != "hello\nworld" {
		t.Errorf("text = %q", text)
	}
	if _, err := ConvertText("slides.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", []byte("PK")); err == nil {
		t.Fatal("ConvertText accepted a format this build cannot parse")
	}
}

// TestConvertTextExtractsFlatePDF builds a minimal text PDF (a Flate-compressed
// content stream with two Tj operators) and checks the extractor reads it.
func TestConvertTextExtractsFlatePDF(t *testing.T) {
	pdf := buildFlatePDF(t, "BT /F1 12 Tf (Hello) Tj (World) Tj ET")
	text, err := ConvertText("report.pdf", "application/pdf", pdf)
	if err != nil {
		t.Fatalf("ConvertText: %v", err)
	}
	if text != "Hello World" {
		t.Errorf("text = %q, want %q", text, "Hello World")
	}
}

func buildArchive(t *testing.T, entries map[string]any) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, payload := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func buildFlatePDF(t *testing.T, content string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatalf("compress: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n1 0 obj << /Length ")
	pdf.WriteString(itoa(compressed.Len()))
	pdf.WriteString(" /Filter /FlateDecode >>\nstream\n")
	pdf.Write(compressed.Bytes())
	pdf.WriteString("\nendstream\nendobj\ntrailer<<>>\n%%EOF\n")
	return pdf.Bytes()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
