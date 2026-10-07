package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// ExportTransferDocuments reads a bank's documents with their chunks and facts.
// Embeddings are not carried: an import re-embeds with the target bank's model.
func (s *Store) ExportTransferDocuments(ctx context.Context, bankID string, docIDs []string) ([]model.TransferDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.banks[bankID]; !ok {
		return nil, ErrBankNotFound
	}
	filter := map[string]bool{}
	for _, id := range docIDs {
		filter[id] = true
	}

	keys := make([]string, 0, len(s.documents))
	for key, doc := range s.documents {
		if doc.BankID != bankID {
			continue
		}
		if len(filter) > 0 && !filter[doc.ID] {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	docs := make([]model.TransferDocument, 0, len(keys))
	for _, key := range keys {
		stored := s.documents[key]
		doc := model.TransferDocument{
			ID:           stored.ID,
			OriginalText: stored.OriginalText,
			Tags:         append([]string{}, stored.Tags...),
		}
		unitIDs := append([]string{}, s.byDoc[key]...)
		sort.Slice(unitIDs, func(i, j int) bool {
			a, b := s.units[unitIDs[i]], s.units[unitIDs[j]]
			if a == nil || b == nil {
				return unitIDs[i] < unitIDs[j]
			}
			if a.CreatedAt != b.CreatedAt {
				return a.CreatedAt < b.CreatedAt
			}
			return a.ID < b.ID
		})
		index := 0
		for _, id := range unitIDs {
			u := s.units[id]
			if u == nil || u.FactType == model.FactObservation {
				continue
			}
			doc.Chunks = append(doc.Chunks, model.TransferChunk{ChunkIndex: index, ChunkText: u.Text})
			index++
			doc.Facts = append(doc.Facts, model.TransferFact{
				SourceID:    u.ID,
				FactType:    u.FactType,
				Text:        u.Text,
				Context:     u.Context,
				Tags:        append([]string{}, u.Tags...),
				MentionedAt: u.MentionedAt,
				CreatedAt:   u.CreatedAt,
				ChunkIndex:  -1,
			})
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// ExportTransferObservations lists the bank's consolidated observations.
func (s *Store) ExportTransferObservations(ctx context.Context, bankID string) ([]model.TransferObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.banks[bankID]; !ok {
		return nil, ErrBankNotFound
	}
	var obs []model.Unit
	for _, u := range s.units {
		if u.BankID == bankID && u.FactType == model.FactObservation {
			obs = append(obs, *u)
		}
	}
	sort.Slice(obs, func(i, j int) bool {
		if obs[i].CreatedAt != obs[j].CreatedAt {
			return obs[i].CreatedAt < obs[j].CreatedAt
		}
		return obs[i].ID < obs[j].ID
	})
	out := make([]model.TransferObservation, 0, len(obs))
	for _, u := range obs {
		out = append(out, model.TransferObservation{
			SourceID:    u.ID,
			Text:        u.Text,
			Tags:        append([]string{}, u.Tags...),
			MentionedAt: u.MentionedAt,
			CreatedAt:   u.CreatedAt,
			ProofCount:  len(u.SourceMemoryIds),
			Sources:     append([]string{}, u.SourceMemoryIds...),
		})
	}
	return out, nil
}

// RestoreTransferDocument writes one archive document into a bank through the
// same retain path a normal write takes, so no facts are invented.
func (s *Store) RestoreTransferDocument(ctx context.Context, bankID string, doc model.TransferDocument, onConflict string) (model.TransferOutcome, error) {
	if err := ctx.Err(); err != nil {
		return model.TransferOutcome{}, err
	}
	s.mu.RLock()
	_, bankOK := s.banks[bankID]
	_, exists := s.documents[bankID+"/"+doc.ID]
	s.mu.RUnlock()
	if !bankOK {
		return model.TransferOutcome{}, ErrBankNotFound
	}

	targetID := doc.ID
	switch {
	case !exists:
	case onConflict == "skip":
		return model.TransferOutcome{Skipped: true}, nil
	case onConflict == "new-id":
		targetID = doc.ID + "-" + model.ContentHash(doc.ID + bankID)[:8]
	case onConflict == "replace":
		s.mu.Lock()
		key := bankID + "/" + doc.ID
		for _, id := range s.byDoc[key] {
			delete(s.units, id)
			delete(s.consolidatedSet, id)
		}
		delete(s.byDoc, key)
		delete(s.documents, key)
		s.mu.Unlock()
	default:
		return model.TransferOutcome{}, fmt.Errorf("invalid on_conflict %q", onConflict)
	}

	items := transferItems(doc, targetID)
	if len(items) == 0 {
		s.mu.Lock()
		s.documents[bankID+"/"+targetID] = &model.Document{
			ID: targetID, BankID: bankID, OriginalText: doc.OriginalText,
			ContentHash: model.ContentHash(doc.OriginalText), Tags: doc.Tags,
		}
		s.mu.Unlock()
		return model.TransferOutcome{}, nil
	}
	res, err := s.Retain(ctx, bankID, items)
	if err != nil {
		return model.TransferOutcome{}, err
	}
	return model.TransferOutcome{CreatedFacts: res.UnitsStored}, nil
}

// RestoreTransferObservation inserts a consolidated observation.
func (s *Store) RestoreTransferObservation(ctx context.Context, bankID string, obs model.TransferObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(obs.Text) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.banks[bankID]; !ok {
		return ErrBankNotFound
	}
	id := model.UnitID(bankID, "observation", obs.Text)
	if _, ok := s.units[id]; ok {
		return nil
	}
	created := obs.CreatedAt
	if created == "" {
		created = s.now().UTC().Format(time.RFC3339)
	}
	s.units[id] = &model.Unit{
		ID:              id,
		BankID:          bankID,
		FactType:        model.FactObservation,
		Text:            obs.Text,
		Tags:            append([]string{}, obs.Tags...),
		MentionedAt:     obs.MentionedAt,
		CreatedAt:       created,
		SourceMemoryIds: append([]string{}, obs.Sources...),
	}
	return nil
}

// transferItems converts an archive document into retain items: extracted facts
// when present, otherwise the raw chunks.
func transferItems(doc model.TransferDocument, targetID string) []model.RetainItem {
	var items []model.RetainItem
	if len(doc.Facts) > 0 {
		for _, f := range doc.Facts {
			if strings.TrimSpace(f.Text) == "" {
				continue
			}
			factType := f.FactType
			if factType != model.FactWorld && factType != model.FactExperience {
				factType = model.FactExperience
			}
			items = append(items, model.RetainItem{
				Content:     f.Text,
				DocumentID:  targetID,
				Tags:        mergeTransferTags(doc.Tags, f.Tags),
				Context:     f.Context,
				MentionedAt: firstTransferTime(f.EventDate, f.OccurredStart, f.MentionedAt),
				FactType:    factType,
			})
		}
		return items
	}
	for _, c := range doc.Chunks {
		if strings.TrimSpace(c.ChunkText) == "" {
			continue
		}
		items = append(items, model.RetainItem{
			Content:    c.ChunkText,
			DocumentID: targetID,
			Tags:       doc.Tags,
			FactType:   model.FactExperience,
		})
	}
	return items
}

func mergeTransferTags(docTags, factTags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range append(append([]string{}, docTags...), factTags...) {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func firstTransferTime(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// BankAttachment: the in-memory backend keeps no attachment bytes, so it
// reports "not found" rather than pretending one exists.
func (s *Store) BankAttachment(ctx context.Context, bankID, attachmentID string) (string, string, error) {
	return "", "", nil
}

// UpdateDirective rewrites the content of the directive with this name,
// creating it when the bank has none.
func (s *Store) UpdateDirective(ctx context.Context, bankID, name, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.banks[bankID]; !ok {
		return ErrBankNotFound
	}
	for _, d := range s.directives[bankID] {
		if d.Name == name {
			d.Content = content
			return nil
		}
	}
	id := model.UnitID(bankID, "directive", name+"\x00"+content)
	s.directives[bankID] = append(s.directives[bankID], &Directive{ID: "dir-" + id[:12], Name: name, Content: content})
	return nil
}
