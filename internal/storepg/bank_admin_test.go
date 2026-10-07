package storepg

import (
	"context"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// TestBankAdminLiteMode covers the bank-admin SQL against the lite schema:
// partial updates, config merge, aliases, and the delete sweep.
func TestBankAdminLiteMode(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const bank = "admin-lite"

	if _, err := s.EnsureBank(ctx, bank, "Admin", ""); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}
	if _, err := s.Retain(ctx, bank, []model.RetainItem{
		{Content: "Alice keeps a notebook.", FactType: model.FactExperience},
	}); err != nil {
		t.Fatalf("retain: %v", err)
	}

	name := "Renamed"
	b, err := s.UpdateBank(ctx, bank, model.BankPatch{
		Name:   &name,
		Config: map[string]any{"reflect_mission": "stay honest"},
	})
	if err != nil {
		t.Fatalf("update bank: %v", err)
	}
	if b == nil || b.Name != "Renamed" {
		t.Fatalf("bank after update = %+v", b)
	}
	cfg, err := s.BankConfig(ctx, bank)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if cfg["reflect_mission"] != "stay honest" {
		t.Errorf("config = %v", cfg)
	}
	if _, err := s.UpdateBank(ctx, "missing-bank", model.BankPatch{}); err != nil {
		t.Fatalf("update missing bank: %v", err)
	}
	if b, _ := s.GetBank(ctx, "missing-bank"); b != nil {
		t.Errorf("UpdateBank created a missing bank")
	}

	if err := s.CreateBankAlias(ctx, bank, "admin-legacy", true); err != nil {
		t.Fatalf("create alias: %v", err)
	}
	if err := s.CreateBankAlias(ctx, bank, "admin-legacy", false); err != model.ErrAliasConflict {
		t.Errorf("duplicate alias = %v, want ErrAliasConflict", err)
	}
	aliases, err := s.ListBankAliases(ctx, bank)
	if err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	if len(aliases) != 1 || aliases[0].Alias != "admin-legacy" || !aliases[0].Primary {
		t.Fatalf("aliases = %+v", aliases)
	}
	if ok, err := s.SetBankAliasPrimary(ctx, bank, "admin-legacy", false); err != nil || !ok {
		t.Fatalf("demote alias = (%v, %v)", ok, err)
	}

	counts, err := s.DeleteBank(ctx, bank)
	if err != nil {
		t.Fatalf("delete bank: %v", err)
	}
	if counts.MemoryUnits != 1 || counts.Documents != 1 {
		t.Errorf("counts = %+v, want one unit and one document", counts)
	}
	if b, _ := s.GetBank(ctx, bank); b != nil {
		t.Errorf("bank survived delete: %+v", b)
	}
	if aliases, _ := s.ListBankAliases(ctx, bank); len(aliases) != 0 {
		t.Errorf("aliases survived delete: %+v", aliases)
	}
	if cfg, _ := s.BankConfig(ctx, bank); len(cfg) != 0 {
		t.Errorf("config survived delete: %v", cfg)
	}
	if counts, err := s.DeleteBank(ctx, bank); err != nil || counts.Total() != 0 {
		t.Errorf("second delete = (%+v, %v), want zeros", counts, err)
	}
}

// TestBankAdminUpstreamMode runs the same surface against the upstream-shaped
// fixture, where config lives in banks.config and aliases in bank_aliases.
func TestBankAdminUpstreamMode(t *testing.T) {
	s, ctx := newUpstreamStore(t)
	const bank = "admin-up"

	if _, err := s.EnsureBank(ctx, bank, "Up", "old mission"); err != nil {
		t.Fatalf("ensure bank: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO documents (id, bank_id, original_text, content_hash) VALUES ('d1', $1, 'text', 'h1')", bank); err != nil {
		t.Fatalf("seed upstream document: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO memory_units (id, bank_id, document_id, text, fact_type, event_date) VALUES (gen_random_uuid(), $1, 'd1', 'Alice keeps a notebook.', 'experience', now())", bank); err != nil {
		t.Fatalf("seed upstream unit: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO entities (canonical_name, bank_id) VALUES ('Alice', $1)", bank); err != nil {
		t.Fatalf("seed upstream entity: %v", err)
	}

	mission := "new mission"
	b, err := s.UpdateBank(ctx, bank, model.BankPatch{
		Mission: &mission,
		Config:  map[string]any{"reflect_mission": "config wins"},
	})
	if err != nil {
		t.Fatalf("update bank: %v", err)
	}
	if b == nil || b.Mission != "new mission" {
		t.Fatalf("bank after update = %+v", b)
	}
	cfg, err := s.BankConfig(ctx, bank)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if cfg["reflect_mission"] != "config wins" {
		t.Errorf("config = %v", cfg)
	}

	if err := s.CreateBankAlias(ctx, bank, "up-legacy", false); err != nil {
		t.Fatalf("create alias: %v", err)
	}
	if err := s.CreateBankAlias(ctx, bank, "up-legacy", false); err != model.ErrAliasConflict {
		t.Errorf("duplicate alias = %v, want ErrAliasConflict", err)
	}
	aliases, err := s.ListBankAliases(ctx, bank)
	if err != nil || len(aliases) != 1 {
		t.Fatalf("aliases = %+v (err %v)", aliases, err)
	}

	counts, err := s.DeleteBank(ctx, bank)
	if err != nil {
		t.Fatalf("delete bank: %v", err)
	}
	if counts.MemoryUnits != 1 || counts.Documents != 1 || counts.Entities != 1 {
		t.Errorf("counts = %+v, want 1 unit, 1 document, 1 entity", counts)
	}
	if b, _ := s.GetBank(ctx, bank); b != nil {
		t.Errorf("bank survived delete: %+v", b)
	}
	var units, docs, entities int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM memory_units WHERE bank_id = $1", bank).Scan(&units); err != nil {
		t.Fatalf("count units: %v", err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM documents WHERE bank_id = $1", bank).Scan(&docs); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM entities WHERE bank_id = $1", bank).Scan(&entities); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if units != 0 || docs != 0 || entities != 0 {
		t.Errorf("rows survived delete: units=%d docs=%d entities=%d", units, docs, entities)
	}
	var aliasCount int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM bank_aliases WHERE bank_id = $1", bank).Scan(&aliasCount); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	if aliasCount != 0 {
		t.Errorf("aliases survived delete: %d", aliasCount)
	}
}
