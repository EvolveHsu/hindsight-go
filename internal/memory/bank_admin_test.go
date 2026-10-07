package memory

import (
	"context"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// TestBankAdminUpdateAndDelete covers the in-process side of the bank-admin
// seam: partial updates, the config overlay merge, and the delete sweep that
// takes every bank-scoped row with it.
func TestBankAdminUpdateAndDelete(t *testing.T) {
	s := New()
	ctx := context.Background()
	if _, err := s.EnsureBank(ctx, "b1", "B1", "old"); err != nil {
		t.Fatalf("ensure b1: %v", err)
	}
	if _, err := s.EnsureBank(ctx, "b2", "B2", ""); err != nil {
		t.Fatalf("ensure b2: %v", err)
	}
	if _, err := s.Retain(ctx, "b1", []model.RetainItem{
		{Content: "Alice keeps a notebook.", FactType: model.FactExperience},
	}); err != nil {
		t.Fatalf("retain: %v", err)
	}
	if _, err := s.CreateDirective(ctx, "b1", "tone", "be terse"); err != nil {
		t.Fatalf("create directive: %v", err)
	}
	if _, err := s.CreateMentalModel(ctx, "b1", &model.MentalModel{ID: "mm1", Name: "n", SourceQuery: "q"}); err != nil {
		t.Fatalf("create mental model: %v", err)
	}
	if err := s.UpdateBankConfig(ctx, "b1", map[string]any{"retain_mission": "extract"}); err != nil {
		t.Fatalf("update config: %v", err)
	}
	if err := s.CreateBankAlias(ctx, "b1", "legacy-b1", false); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	// A partial update touches only the named fields.
	newName := "Renamed"
	b, err := s.UpdateBank(ctx, "b1", model.BankPatch{Name: &newName})
	if err != nil {
		t.Fatalf("update bank: %v", err)
	}
	if b == nil || b.Name != "Renamed" || b.Mission != "old" {
		t.Fatalf("bank after patch = %+v", b)
	}
	if _, err := s.UpdateBank(ctx, "b1", model.BankPatch{Config: map[string]any{"enable_observations": true}}); err != nil {
		t.Fatalf("update bank config: %v", err)
	}
	cfg, err := s.BankConfig(ctx, "b1")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if cfg["retain_mission"] != "extract" || cfg["enable_observations"] != true {
		t.Errorf("config = %v, want the merged overrides", cfg)
	}

	// Updating a bank that does not exist reports nil instead of creating it.
	if got, err := s.UpdateBank(ctx, "ghost", model.BankPatch{}); err != nil || got != nil {
		t.Errorf("update missing bank = (%v, %v), want (nil, nil)", got, err)
	}

	counts, err := s.DeleteBank(ctx, "b1")
	if err != nil {
		t.Fatalf("delete bank: %v", err)
	}
	if counts.MemoryUnits != 1 || counts.Documents != 1 {
		t.Errorf("counts = %+v, want one unit and one document", counts)
	}
	if b, _ := s.GetBank(ctx, "b1"); b != nil {
		t.Errorf("bank still present after delete: %+v", b)
	}
	if b, _ := s.GetBank(ctx, "b2"); b == nil {
		t.Errorf("delete removed the wrong bank")
	}
	aliases, _ := s.ListBankAliases(ctx, "b1")
	if len(aliases) != 0 {
		t.Errorf("aliases after delete = %v", aliases)
	}
	if cfg, _ := s.BankConfig(ctx, "b1"); len(cfg) != 0 {
		t.Errorf("config after delete = %v", cfg)
	}
	if mm, _ := s.GetMentalModel(ctx, "b1", "mm1"); mm != nil {
		t.Errorf("mental model survived delete: %+v", mm)
	}
	if ds, _ := s.ListDirectives(ctx, "b1"); len(ds) != 0 {
		t.Errorf("directives survived delete: %v", ds)
	}
	if ids, _ := s.BankIDs(ctx); len(ids) != 1 || ids[0] != "b2" {
		t.Errorf("bank ids after delete = %v", ids)
	}

	// Deleting a bank that is already gone is a no-op reporting zero.
	counts, err = s.DeleteBank(ctx, "b1")
	if err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if counts.Total() != 0 {
		t.Errorf("second delete counts = %+v, want zero", counts)
	}
}

// TestBankAliasOrderAndConflicts pins the alias rules: primary first, an alias
// may not shadow a bank or repeat, and promotion demotes the previous primary.
func TestBankAliasOrderAndConflicts(t *testing.T) {
	s := New()
	ctx := context.Background()
	if _, err := s.EnsureBank(ctx, "b1", "B1", ""); err != nil {
		t.Fatalf("ensure b1: %v", err)
	}
	if _, err := s.EnsureBank(ctx, "b2", "B2", ""); err != nil {
		t.Fatalf("ensure b2: %v", err)
	}
	if err := s.CreateBankAlias(ctx, "b1", "a1", false); err != nil {
		t.Fatalf("create a1: %v", err)
	}
	if err := s.CreateBankAlias(ctx, "b1", "a2", true); err != nil {
		t.Fatalf("create a2: %v", err)
	}
	if err := s.CreateBankAlias(ctx, "b1", "b2", false); err != model.ErrAliasConflict {
		t.Errorf("alias shadowing a bank = %v, want ErrAliasConflict", err)
	}
	if err := s.CreateBankAlias(ctx, "b1", "a1", false); err != model.ErrAliasConflict {
		t.Errorf("duplicate alias = %v, want ErrAliasConflict", err)
	}

	aliases, err := s.ListBankAliases(ctx, "b1")
	if err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	if len(aliases) != 2 || aliases[0].Alias != "a2" || !aliases[0].Primary {
		t.Fatalf("aliases = %+v, want the primary first", aliases)
	}

	ok, err := s.SetBankAliasPrimary(ctx, "b1", "a1", true)
	if err != nil || !ok {
		t.Fatalf("promote a1 = (%v, %v)", ok, err)
	}
	aliases, _ = s.ListBankAliases(ctx, "b1")
	if aliases[0].Alias != "a1" || !aliases[0].Primary || aliases[1].Primary {
		t.Errorf("aliases after promote = %+v", aliases)
	}
	if ok, _ := s.SetBankAliasPrimary(ctx, "b1", "missing", true); ok {
		t.Errorf("promoting a missing alias reported success")
	}

	removed, err := s.DeleteBankAlias(ctx, "b1", "a1")
	if err != nil || !removed {
		t.Fatalf("delete a1 = (%v, %v)", removed, err)
	}
	if removed, _ := s.DeleteBankAlias(ctx, "b1", "a1"); removed {
		t.Errorf("second delete reported success")
	}
}
