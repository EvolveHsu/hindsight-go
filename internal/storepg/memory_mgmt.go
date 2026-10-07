package storepg

import (
	"context"
	"fmt"
	"sort"

	"github.com/EvolveHsu/hindsight-go/internal/embeddings"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// ListMemories returns units for a bank with pagination (newest first).
func (s *Store) ListMemories(ctx context.Context, bankID string, limit, offset int) ([]model.Unit, int, error) {
	ut := s.unitsTable()
	rows, err := s.pool.Query(ctx,
		"SELECT id::text, bank_id, COALESCE(document_id,''), fact_type, text, COALESCE(context,''), COALESCE(chunk_id,''), COALESCE(tags::text[],'{}'), COALESCE(to_char(created_at, 'YYYY-MM-DD\"T\"HH24:MI:SSTZH:TZM'), '') FROM "+ut+" WHERE bank_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3",
		bankID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.Unit
	for rows.Next() {
		u := &model.Unit{}
		var tags []string
		if err := rows.Scan(&u.ID, &u.BankID, &u.DocumentID, &u.FactType, &u.Text, &u.Context, &u.ChunkID, &tags, &u.CreatedAt); err != nil {
			return nil, 0, err
		}
		u.Tags = tags
		out = append(out, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM "+ut+" WHERE bank_id = $1", bankID).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (s *Store) GetMemory(ctx context.Context, bankID, memoryID string) (*model.Unit, error) {
	ut := s.unitsTable()
	row := s.pool.QueryRow(ctx,
		"SELECT id::text, bank_id, COALESCE(document_id,''), fact_type, text, COALESCE(context,''), COALESCE(chunk_id,''), COALESCE(tags::text[],'{}'), COALESCE(to_char(created_at, 'YYYY-MM-DD\"T\"HH24:MI:SSTZH:TZM'), '') FROM "+ut+" WHERE id = $1 AND bank_id = $2",
		memoryID, bankID)
	u := &model.Unit{}
	var tags []string
	if err := row.Scan(&u.ID, &u.BankID, &u.DocumentID, &u.FactType, &u.Text, &u.Context, &u.ChunkID, &tags, &u.CreatedAt); err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	u.Tags = tags
	return u, nil
}

func (s *Store) UpdateMemory(ctx context.Context, bankID, memoryID string, fn func(*model.Unit)) (*model.Unit, error) {
	u, err := s.GetMemory(ctx, bankID, memoryID)
	if err != nil || u == nil {
		return nil, err
	}
	fn(u)
	var embedding string
	if s.embedder != nil {
		vectors, embedErr := s.embedder.Embed(ctx, []string{u.Text}, embeddings.InputDocument)
		if embedErr != nil {
			return nil, embedErr
		}
		if len(vectors) != 1 || len(vectors[0]) == 0 {
			return nil, fmt.Errorf("embed updated memory %q: provider returned no vector", memoryID)
		}
		embedding = embeddingLiteral(vectors[0])
	}
	ut := s.unitsTable()
	query := "UPDATE " + ut + " SET text = $3, context = $4, fact_type = $5"
	args := []any{memoryID, bankID, u.Text, u.Context, string(u.FactType)}
	if embedding != "" {
		query += ", embedding = '" + embedding + "'"
		if s.upstream && s.pgvector {
			query += "::vector"
		}
	}
	if s.upstream {
		query += ", edited_at = now(), updated_at = now()"
	}
	query += " WHERE id = $1 AND bank_id = $2"
	_, err = s.pool.Exec(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) InvalidateMemory(ctx context.Context, bankID, memoryID, reason string) error {
	return nil // lite: no invalidation support
}

func (s *Store) ClearBankMemories(ctx context.Context, bankID string) (int, error) {
	ut := s.unitsTable()
	tag, err := s.pool.Exec(ctx, "DELETE FROM "+ut+" WHERE bank_id = $1", bankID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) ClearMemoryObservations(ctx context.Context, bankID, memoryID string) (int, error) {
	ut := s.unitsTable()
	sourceCol := s.sourceIDsColumn()
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM "+ut+" WHERE bank_id = $1 AND fact_type = 'observation' AND $2 = ANY("+sourceCol+"::text[])",
		bankID, memoryID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Store) ListDocuments(ctx context.Context, bankID string, limit, offset int) ([]model.DocumentInfo, int, error) {
	dt := s.docsTable()
	rows, err := s.pool.Query(ctx,
		"SELECT id, bank_id, COALESCE(original_text,''), COALESCE(content_hash,''), COALESCE(tags::text[],'{}'), to_char(created_at, 'YYYY-MM-DD\"T\"HH24:MI:SSTZH:TZM') FROM "+dt+" WHERE bank_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3",
		bankID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.DocumentInfo
	for rows.Next() {
		d := model.DocumentInfo{}
		if err := rows.Scan(&d.ID, &d.BankID, &d.OriginalText, &d.ContentHash, &d.Tags, &d.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+dt+" WHERE bank_id = $1", bankID).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (s *Store) GetDocument(ctx context.Context, bankID, docID string) (*model.DocumentInfo, error) {
	dt := s.docsTable()
	row := s.pool.QueryRow(ctx,
		"SELECT id, bank_id, COALESCE(original_text,''), COALESCE(content_hash,''), COALESCE(tags::text[],'{}'), to_char(created_at, 'YYYY-MM-DD\"T\"HH24:MI:SSTZH:TZM') FROM "+dt+" WHERE id = $1 AND bank_id = $2",
		docID, bankID)
	d := &model.DocumentInfo{}
	if err := row.Scan(&d.ID, &d.BankID, &d.OriginalText, &d.ContentHash, &d.Tags, &d.CreatedAt); err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (s *Store) DeleteDocument(ctx context.Context, bankID, docID string) (bool, error) {
	ut := s.unitsTable()
	dt := s.docsTable()
	_, err := s.pool.Exec(ctx, "DELETE FROM "+ut+" WHERE bank_id = $1 AND document_id = $2", bankID, docID)
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, "DELETE FROM "+dt+" WHERE bank_id = $1 AND id = $2", bankID, docID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

var _ = sort.Slice
