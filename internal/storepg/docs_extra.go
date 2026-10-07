package storepg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// DocumentChunks implements GET /documents/{document_id}/chunks. Upstream reads
// the `chunks` table retain writes; the lite schema has no such table, so its
// chunks are derived from the retained units one-for-one (the lite retain stores
// each item verbatim, which is exactly what a chunk is there).
func (s *Store) DocumentChunks(ctx context.Context, bankID, docID string, limit, offset int) (*model.ChunkPage, error) {
	var exists string
	err := s.pool.QueryRow(ctx, "SELECT id FROM documents WHERE id = $1 AND bank_id = $2", docID, bankID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	page := &model.ChunkPage{Limit: limit, Offset: offset}
	if s.upstream {
		if err := s.pool.QueryRow(ctx,
			"SELECT count(*) FROM chunks WHERE document_id = $1 AND bank_id = $2", docID, bankID).Scan(&page.Total); err != nil {
			return nil, err
		}
		rows, err := s.pool.Query(ctx, fmt.Sprintf(`
			SELECT chunk_id, document_id, bank_id, chunk_index, chunk_text, COALESCE(to_char(created_at, %[1]s), '')
			FROM chunks
			WHERE document_id = $1 AND bank_id = $2
			ORDER BY chunk_index ASC
			LIMIT $3 OFFSET $4`, graphTSFormat), docID, bankID, limit, offset)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var c model.ChunkInfo
			if err := rows.Scan(&c.ChunkID, &c.DocumentID, &c.BankID, &c.ChunkIndex, &c.ChunkText, &c.CreatedAt); err != nil {
				return nil, err
			}
			page.Items = append(page.Items, c)
		}
		if page.Items == nil {
			page.Items = []model.ChunkInfo{}
		}
		return page, rows.Err()
	}

	// Lite: one chunk per retained unit, indexed in creation order.
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM units WHERE bank_id = $1 AND document_id = $2", bankID, docID).Scan(&page.Total); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		WITH ranked AS (
			SELECT id::text AS id, COALESCE(chunk_id, '') AS chunk_id, bank_id, document_id, text,
			       COALESCE(to_char(created_at, %[1]s), '') AS created_at,
			       (row_number() OVER (PARTITION BY bank_id, document_id ORDER BY created_at, id)) - 1 AS chunk_index
			FROM units
			WHERE bank_id = $1 AND document_id = $2
		)
		SELECT chunk_id, id, bank_id, document_id, chunk_index, text, created_at
		FROM ranked
		ORDER BY chunk_index
		LIMIT $3 OFFSET $4`, graphTSFormat), bankID, docID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c model.ChunkInfo
		var unitID string
		if err := rows.Scan(&c.ChunkID, &unitID, &c.BankID, &c.DocumentID, &c.ChunkIndex, &c.ChunkText, &c.CreatedAt); err != nil {
			return nil, err
		}
		if c.ChunkID == "" {
			c.ChunkID = "chunk-" + unitID
		}
		page.Items = append(page.Items, c)
	}
	if page.Items == nil {
		page.Items = []model.ChunkInfo{}
	}
	return page, rows.Err()
}

// Chunk implements GET /default/chunks/{chunk_id}.
func (s *Store) Chunk(ctx context.Context, chunkID string) (*model.ChunkInfo, error) {
	if s.upstream {
		row := s.pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT chunk_id, document_id, bank_id, chunk_index, chunk_text, COALESCE(to_char(created_at, %[1]s), '')
			FROM chunks WHERE chunk_id = $1`, graphTSFormat), chunkID)
		c := &model.ChunkInfo{}
		if err := row.Scan(&c.ChunkID, &c.DocumentID, &c.BankID, &c.ChunkIndex, &c.ChunkText, &c.CreatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		return c, nil
	}
	row := s.pool.QueryRow(ctx, fmt.Sprintf(`
		WITH ranked AS (
			SELECT id::text AS id, COALESCE(chunk_id, '') AS chunk_id, bank_id, document_id, text,
			       COALESCE(to_char(created_at, %[1]s), '') AS created_at,
			       (row_number() OVER (PARTITION BY bank_id, document_id ORDER BY created_at, id)) - 1 AS chunk_index
			FROM units
		)
		SELECT chunk_id, id, bank_id, document_id, chunk_index, text, created_at
		FROM ranked
		WHERE chunk_id = $1 OR ('chunk-' || id) = $1
		LIMIT 1`, graphTSFormat), chunkID)
	c := &model.ChunkInfo{}
	var unitID string
	if err := row.Scan(&c.ChunkID, &unitID, &c.BankID, &c.DocumentID, &c.ChunkIndex, &c.ChunkText, &c.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if c.ChunkID == "" {
		c.ChunkID = "chunk-" + unitID
	}
	return c, nil
}

// UpdateDocumentTags replaces the document's tags, pushes them down to its
// memory units and drops the observations derived from those units so they are
// re-consolidated under the new scope. Returns false when the document is gone.
func (s *Store) UpdateDocumentTags(ctx context.Context, bankID, docID string, tags []string) (bool, error) {
	ut := s.unitsTable()
	tags = normalizeTags(tags)

	var found bool
	if s.upstream {
		var id string
		err := s.pool.QueryRow(ctx,
			"UPDATE documents SET tags = $3::varchar[], updated_at = now() WHERE id = $1 AND bank_id = $2 RETURNING id",
			docID, bankID, tags).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		found = true
		// Observations built on this document's facts no longer match the new
		// tag scope; upstream invalidates and re-consolidates them.
		srcCol := s.sourceIDsColumn()
		if _, err := s.pool.Exec(ctx, fmt.Sprintf(`
			DELETE FROM %[1]s
			WHERE bank_id = $1 AND fact_type = 'observation' AND %[2]s && (
				SELECT COALESCE(array_agg(id), '{}'::uuid[]) FROM %[1]s
				WHERE bank_id = $1 AND document_id = $2 AND fact_type IN ('world', 'experience')
			)`, ut, srcCol), bankID, docID); err != nil {
			return false, err
		}
		if _, err := s.pool.Exec(ctx,
			fmt.Sprintf("UPDATE %s SET tags = $3::varchar[], updated_at = now() WHERE document_id = $1 AND bank_id = $2", ut),
			docID, bankID, tags); err != nil {
			return false, err
		}
		return found, nil
	}

	var id string
	err := s.pool.QueryRow(ctx,
		"UPDATE documents SET tags = $3 WHERE id = $1 AND bank_id = $2 RETURNING id", docID, bankID, tags).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	found = true
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %[1]s
		WHERE bank_id = $1 AND fact_type = 'observation' AND source_fact_ids && (
			SELECT COALESCE(array_agg(id), '{}'::text[]) FROM %[1]s
			WHERE bank_id = $1 AND document_id = $2 AND fact_type IN ('world', 'experience')
		)`, ut), bankID, docID); err != nil {
		return false, err
	}
	if _, err := s.pool.Exec(ctx,
		fmt.Sprintf("UPDATE %s SET tags = $3 WHERE document_id = $1 AND bank_id = $2", ut), docID, bankID, tags); err != nil {
		return false, err
	}
	return found, nil
}

// ReprocessDocument implements POST /documents/{document_id}/reprocess. The lite
// extractor is content-deterministic (retain stores each item verbatim and keys
// units on its content), so re-running it over an unchanged document produces
// the units that already exist. This refreshes the pipeline timestamps and
// reports the unit count instead of deleting and rebuilding rows, which would
// drop the entity postings and links that hang off those unit ids.
func (s *Store) ReprocessDocument(ctx context.Context, bankID, docID string) (*model.ReprocessResult, error) {
	var text string
	err := s.pool.QueryRow(ctx,
		"SELECT COALESCE(original_text, '') FROM documents WHERE id = $1 AND bank_id = $2", docID, bankID).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		// Upstream treats a document without stored text as not reprocessable and
		// answers 404 to the caller.
		return nil, nil
	}
	ut := s.unitsTable()
	var n int
	if err := s.pool.QueryRow(ctx,
		fmt.Sprintf("SELECT count(*) FROM %s WHERE bank_id = $1 AND document_id = $2", ut), bankID, docID).Scan(&n); err != nil {
		return nil, err
	}
	if s.upstream {
		if _, err := s.pool.Exec(ctx,
			fmt.Sprintf("UPDATE %s SET updated_at = now() WHERE bank_id = $1 AND document_id = $2", ut), bankID, docID); err != nil {
			return nil, err
		}
		if _, err := s.pool.Exec(ctx,
			"UPDATE documents SET updated_at = now() WHERE id = $1 AND bank_id = $2", docID, bankID); err != nil {
			return nil, err
		}
	}
	return &model.ReprocessResult{
		OperationID: fmt.Sprintf("reprocess-%s-%d", docID, time.Now().UnixNano()),
		ItemsCount:  n,
	}, nil
}
