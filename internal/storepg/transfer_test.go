package storepg

import (
	"context"
	"strings"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// TestTransferExportRestoreRoundTrip exercises the SQL behind the document and
// bank transfer endpoints against a real database: export reads documents,
// chunks, facts and observations; restore writes them through the retain path.
func TestTransferExportRestoreRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const (
		source = "it-transfer-source"
		target = "it-transfer-target"
	)

	for _, bank := range []string{source, target} {
		if _, err := s.EnsureBank(ctx, bank, bank, ""); err != nil {
			t.Fatalf("ensure %s: %v", bank, err)
		}
	}
	if _, err := s.Retain(ctx, source, []model.RetainItem{
		{Content: "Ada prefers tea over coffee", DocumentID: "notes", Tags: []string{"person:ada"}, Context: "conversation", FactType: model.FactExperience},
		{Content: "Ada works on embeddings", DocumentID: "notes", Tags: []string{"person:ada"}, FactType: model.FactWorld},
	}); err != nil {
		t.Fatalf("retain: %v", err)
	}

	docs, err := s.ExportTransferDocuments(ctx, source, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("documents = %d, want 1", len(docs))
	}
	doc := docs[0]
	if doc.ID != "notes" || len(doc.Facts) != 2 || len(doc.Chunks) != 2 {
		t.Fatalf("exported document = %+v", doc)
	}
	if doc.Facts[0].FactType == "" || len(doc.Facts[0].Tags) == 0 {
		t.Errorf("fact lost its type or tags: %+v", doc.Facts[0])
	}

	// First restore writes both facts; a replay is skipped.
	out, err := s.RestoreTransferDocument(ctx, target, doc, "skip")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if out.Skipped || out.CreatedFacts != 2 {
		t.Fatalf("restore outcome = %+v, want 2 created facts", out)
	}
	again, err := s.RestoreTransferDocument(ctx, target, doc, "skip")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.Skipped {
		t.Errorf("replay outcome = %+v, want skipped", again)
	}

	// replace rewrites the document in place; new-id keeps both copies.
	replaced, err := s.RestoreTransferDocument(ctx, target, doc, "replace")
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if replaced.Skipped || replaced.CreatedFacts != 2 {
		t.Errorf("replace outcome = %+v", replaced)
	}
	renamed, err := s.RestoreTransferDocument(ctx, target, doc, "new-id")
	if err != nil {
		t.Fatalf("new-id: %v", err)
	}
	if renamed.Skipped || renamed.CreatedFacts != 2 {
		t.Errorf("new-id outcome = %+v", renamed)
	}
	exported, err := s.ExportTransferDocuments(ctx, target, nil)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if len(exported) != 2 {
		t.Fatalf("target documents = %d, want the original plus the new id", len(exported))
	}

	// Observations travel separately.
	if err := s.RestoreTransferObservation(ctx, target, model.TransferObservation{
		Text: "Ada drinks tea", Tags: []string{"person:ada"}, ProofCount: 2,
	}); err != nil {
		t.Fatalf("restore observation: %v", err)
	}
	obs, err := s.ExportTransferObservations(ctx, target)
	if err != nil {
		t.Fatalf("export observations: %v", err)
	}
	if len(obs) != 1 || obs[0].Text != "Ada drinks tea" {
		t.Fatalf("observations = %+v", obs)
	}
	if _, _, err := s.BankAttachment(ctx, target, "missing"); err != nil {
		t.Errorf("BankAttachment on a missing id: %v", err)
	}
}

// TestUpdateDirectiveRewritesByName pins the template-import helper: the row is
// updated when the name exists and created when it does not.
func TestUpdateDirectiveRewritesByName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const bank = "it-transfer-directives"

	if _, err := s.EnsureBank(ctx, bank, bank, ""); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := s.UpdateDirective(ctx, bank, "tone", "be warm"); err != nil {
		t.Fatalf("create via update: %v", err)
	}
	if err := s.UpdateDirective(ctx, bank, "tone", "be brief"); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, err := s.ListDirectives(ctx, bank)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || strings.TrimSpace(list[0].Content) != "be brief" {
		t.Fatalf("directives = %+v, want a single updated row", list)
	}
}
