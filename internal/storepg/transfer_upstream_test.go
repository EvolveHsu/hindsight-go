package storepg

import (
	"strings"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// TestUpstreamTransferRoundTrip runs the transfer projections against the
// upstream table shapes (memory_units with occurred_start/mentioned_at, a real
// chunks table), which the lite fixture does not have.
func TestUpstreamTransferRoundTrip(t *testing.T) {
	s, ctx := newUpstreamStore(t)
	const (
		source = "up-transfer-source"
		target = "up-transfer-target"
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

	docs, err := s.ExportTransferDocuments(ctx, source, []string{"notes"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "notes" {
		t.Fatalf("documents = %+v", docs)
	}
	if len(docs[0].Facts) != 2 {
		t.Fatalf("facts = %d, want 2", len(docs[0].Facts))
	}
	if docs[0].Facts[0].FactType != model.FactExperience || docs[0].Facts[1].FactType != model.FactWorld {
		t.Errorf("fact types = %q, %q", docs[0].Facts[0].FactType, docs[0].Facts[1].FactType)
	}
	if docs[0].Facts[0].CreatedAt == "" {
		t.Errorf("fact lost its created_at: %+v", docs[0].Facts[0])
	}

	// A document subset that names nothing yields an empty slice, not an error.
	none, err := s.ExportTransferDocuments(ctx, source, []string{"missing"})
	if err != nil {
		t.Fatalf("subset export: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("subset export = %+v, want none", none)
	}

	out, err := s.RestoreTransferDocument(ctx, target, docs[0], "skip")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if out.Skipped || out.CreatedFacts != 2 {
		t.Fatalf("restore outcome = %+v", out)
	}
	restored, err := s.ExportTransferDocuments(ctx, target, nil)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if len(restored) != 1 || len(restored[0].Facts) != 2 {
		t.Fatalf("restored documents = %+v", restored)
	}
	if !strings.Contains(restored[0].Facts[0].Text, "tea") {
		t.Errorf("restored fact text = %q", restored[0].Facts[0].Text)
	}

	if err := s.RestoreTransferObservation(ctx, target, model.TransferObservation{
		Text: "Ada drinks tea", Tags: []string{"person:ada"},
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

	// The upstream fixture has no attachments table; a read answers "not
	// found" rather than failing.
	key, _, err := s.BankAttachment(ctx, target, "missing")
	if err != nil {
		t.Fatalf("BankAttachment: %v", err)
	}
	if key != "" {
		t.Errorf("attachment key = %q, want empty", key)
	}
}
