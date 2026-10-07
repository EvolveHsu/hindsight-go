package memory

import (
	"context"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// bankAliasRow keeps one alias plus its display flag; the slice preserves
// creation order so listing matches upstream's "primary first, then oldest".
type bankAliasRow struct {
	alias   string
	primary bool
}

// UpdateBank applies a partial bank update. Config overrides merge shallowly,
// the way upstream's `config || updates` does.
func (s *Store) UpdateBank(ctx context.Context, bankID string, patch model.BankPatch) (*model.Bank, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.banks[bankID]
	if !ok {
		return nil, nil
	}
	if patch.Name != nil {
		b.Name = *patch.Name
	}
	if patch.Mission != nil {
		b.Mission = *patch.Mission
	}
	if len(patch.Config) > 0 {
		if s.bankConfigs == nil {
			s.bankConfigs = map[string]map[string]any{}
		}
		cfg := s.bankConfigs[bankID]
		if cfg == nil {
			cfg = map[string]any{}
			s.bankConfigs[bankID] = cfg
		}
		for k, v := range patch.Config {
			cfg[k] = v
		}
	}
	cp := *b
	return &cp, nil
}

// DeleteBank removes the bank row and every bank-scoped row the lite store
// keeps, mirroring the upstream CASCADE sweep.
func (s *Store) DeleteBank(ctx context.Context, bankID string) (model.BankDeleteCounts, error) {
	if err := ctx.Err(); err != nil {
		return model.BankDeleteCounts{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var counts model.BankDeleteCounts
	if _, ok := s.banks[bankID]; !ok {
		return counts, nil
	}
	for id, u := range s.units {
		if u.BankID == bankID {
			counts.MemoryUnits++
			delete(s.units, id)
		}
	}
	for key, d := range s.documents {
		if d.BankID == bankID {
			counts.Documents++
			delete(s.documents, key)
			delete(s.byDoc, key)
		}
	}
	delete(s.banks, bankID)
	delete(s.bankConfigs, bankID)
	delete(s.aliases, bankID)
	for id, m := range s.mentalModels {
		if m.BankID == bankID {
			delete(s.mentalModels, id)
		}
	}
	delete(s.directives, bankID)
	return counts, nil
}

// BankConfig returns a copy of the bank's overrides.
func (s *Store) BankConfig(ctx context.Context, bankID string) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := s.bankConfigs[bankID]
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
}

// UpdateBankConfig merges overrides into the bank's config map.
func (s *Store) UpdateBankConfig(ctx context.Context, bankID string, updates map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.banks[bankID]; !ok {
		return ErrBankNotFound
	}
	if s.bankConfigs == nil {
		s.bankConfigs = map[string]map[string]any{}
	}
	cfg := s.bankConfigs[bankID]
	if cfg == nil {
		cfg = map[string]any{}
		s.bankConfigs[bankID] = cfg
	}
	for k, v := range updates {
		cfg[k] = v
	}
	return nil
}

// ResetBankConfig drops every override for the bank.
func (s *Store) ResetBankConfig(ctx context.Context, bankID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.banks[bankID]; !ok {
		return ErrBankNotFound
	}
	delete(s.bankConfigs, bankID)
	return nil
}

// ListBankAliases returns the bank's aliases, primary first then oldest first.
func (s *Store) ListBankAliases(ctx context.Context, bankID string) ([]model.BankAlias, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := s.aliases[bankID]
	var primary, rest []model.BankAlias
	for _, row := range rows {
		a := model.BankAlias{Alias: row.alias, Primary: row.primary}
		if row.primary {
			primary = append(primary, a)
			continue
		}
		rest = append(rest, a)
	}
	return append(primary, rest...), nil
}

// CreateBankAlias adds an alias unless it already names a bank or an alias.
func (s *Store) CreateBankAlias(ctx context.Context, bankID, alias string, primary bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.banks[bankID]; !ok {
		return ErrBankNotFound
	}
	if _, taken := s.banks[alias]; taken {
		return model.ErrAliasConflict
	}
	for _, rows := range s.aliases {
		for _, row := range rows {
			if row.alias == alias {
				return model.ErrAliasConflict
			}
		}
	}
	if s.aliases == nil {
		s.aliases = map[string][]bankAliasRow{}
	}
	if primary {
		rows := s.aliases[bankID]
		for i := range rows {
			rows[i].primary = false
		}
		s.aliases[bankID] = rows
	}
	s.aliases[bankID] = append(s.aliases[bankID], bankAliasRow{alias: alias, primary: primary})
	return nil
}

// DeleteBankAlias detaches one alias; false when the bank has no such alias.
func (s *Store) DeleteBankAlias(ctx context.Context, bankID, alias string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.banks[bankID]; !ok {
		return false, ErrBankNotFound
	}
	rows := s.aliases[bankID]
	for i, row := range rows {
		if row.alias == alias {
			s.aliases[bankID] = append(rows[:i:i], rows[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// SetBankAliasPrimary promotes or demotes one alias; false when absent.
func (s *Store) SetBankAliasPrimary(ctx context.Context, bankID, alias string, primary bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.banks[bankID]; !ok {
		return false, ErrBankNotFound
	}
	rows := s.aliases[bankID]
	found := false
	for i := range rows {
		if rows[i].alias == alias {
			found = true
			continue
		}
		if primary {
			rows[i].primary = false
		}
	}
	if !found {
		return false, nil
	}
	for i := range rows {
		if rows[i].alias == alias {
			rows[i].primary = primary
		}
	}
	s.aliases[bankID] = rows
	return true, nil
}
