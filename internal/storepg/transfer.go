package storepg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// transferTSFormat renders timestamps as RFC3339 strings on the wire.
const transferTSFormat = `'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM'`

// transferColumns adapts the export projections to the schema in use. The
// lite tables carry fewer columns than upstream's memory_units/documents.
func (s *Store) documentTimestampExpr() string {
	if s.upstream {
		return fmt.Sprintf("COALESCE(to_char(created_at, %s), '')", transferTSFormat)
	}
	return "''"
}

func (s *Store) transferFactsQuery() string {
	if s.upstream {
		return fmt.Sprintf(`
SELECT id::text, fact_type, text, COALESCE(context,''), COALESCE(tags::text[],'{}'),
       COALESCE(to_char(occurred_start, %[1]s), ''),
       COALESCE(to_char(occurred_end,   %[1]s), ''),
       COALESCE(to_char(mentioned_at,   %[1]s), ''),
       COALESCE(to_char(created_at,     %[1]s), '')
FROM memory_units
WHERE bank_id = $1 AND document_id = $2 AND fact_type <> 'observation'
ORDER BY created_at, id`, transferTSFormat)
	}
	return fmt.Sprintf(`
SELECT id::text, fact_type, text, COALESCE(context,''), COALESCE(tags::text[],'{}'),
       '', '', '', COALESCE(to_char(created_at, %s), '')
FROM units
WHERE bank_id = $1 AND document_id = $2 AND fact_type <> 'observation'
ORDER BY created_at, id`, transferTSFormat)
}

// ExportTransferDocuments reads a bank's documents with their chunks and facts.
// Embeddings and row ids are not carried: an import re-embeds with the target
// bank's model, exactly like upstream.
func (s *Store) ExportTransferDocuments(ctx context.Context, bankID string, docIDs []string) ([]model.TransferDocument, error) {
	query := fmt.Sprintf(`
SELECT id, COALESCE(original_text,''), COALESCE(tags::text[],'{}'), %s
FROM documents
WHERE bank_id = $1`, s.documentTimestampExpr())
	args := []any{bankID}
	if len(docIDs) > 0 {
		query += " AND id = ANY($2::text[])"
		args = append(args, docIDs)
	}
	query += " ORDER BY id"

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []model.TransferDocument
	for rows.Next() {
		var doc model.TransferDocument
		var tags []string
		if err := rows.Scan(&doc.ID, &doc.OriginalText, &tags, &doc.CreatedAt); err != nil {
			return nil, err
		}
		doc.Tags = tags
		chunks, err := s.transferChunks(ctx, bankID, doc.ID)
		if err != nil {
			return nil, err
		}
		doc.Chunks = chunks
		facts, err := s.transferFacts(ctx, bankID, doc.ID)
		if err != nil {
			return nil, err
		}
		doc.Facts = facts
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

func (s *Store) transferChunks(ctx context.Context, bankID, docID string) ([]model.TransferChunk, error) {
	if s.upstream {
		rows, err := s.pool.Query(ctx,
			`SELECT chunk_index, chunk_text FROM chunks WHERE bank_id = $1 AND document_id = $2 ORDER BY chunk_index`,
			bankID, docID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []model.TransferChunk
		for rows.Next() {
			var c model.TransferChunk
			if err := rows.Scan(&c.ChunkIndex, &c.ChunkText); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, rows.Err()
	}
	// Lite: one chunk per retained unit, in creation order.
	rows, err := s.pool.Query(ctx,
		`SELECT (row_number() OVER (ORDER BY created_at, id)) - 1 AS idx, text
		 FROM units WHERE bank_id = $1 AND document_id = $2 ORDER BY created_at, id`,
		bankID, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TransferChunk
	for rows.Next() {
		var c model.TransferChunk
		if err := rows.Scan(&c.ChunkIndex, &c.ChunkText); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) transferFacts(ctx context.Context, bankID, docID string) ([]model.TransferFact, error) {
	rows, err := s.pool.Query(ctx, s.transferFactsQuery(), bankID, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TransferFact
	for rows.Next() {
		f := model.TransferFact{ChunkIndex: -1}
		var tags []string
		if err := rows.Scan(&f.SourceID, &f.FactType, &f.Text, &f.Context, &tags,
			&f.OccurredStart, &f.OccurredEnd, &f.MentionedAt, &f.CreatedAt); err != nil {
			return nil, err
		}
		f.Tags = tags
		out = append(out, f)
	}
	return out, rows.Err()
}

// ExportTransferObservations lists the bank's consolidated observations.
func (s *Store) ExportTransferObservations(ctx context.Context, bankID string) ([]model.TransferObservation, error) {
	if err := s.ensureConsolidationColumns(ctx); err != nil {
		return nil, err
	}
	srcCol := s.sourceIDsColumn()
	query := fmt.Sprintf(`
SELECT id::text, text, COALESCE(tags::text[],'{}'),
       COALESCE(to_char(mentioned_at, %[1]s), ''),
       COALESCE(to_char(created_at,   %[1]s), ''),
       COALESCE(%[2]s::text[], '{}')
FROM %[3]s
WHERE bank_id = $1 AND fact_type = 'observation'
ORDER BY created_at, id`, transferTSFormat, srcCol, s.unitsTable())
	if !s.upstream {
		// The lite table has no mentioned_at column; the source array column is
		// text[] rather than uuid[].
		query = fmt.Sprintf(`
SELECT id::text, text, COALESCE(tags::text[],'{}'), '',
       COALESCE(to_char(created_at, %[1]s), ''),
       COALESCE(%[2]s, '{}')
FROM %[3]s
WHERE bank_id = $1 AND fact_type = 'observation'
ORDER BY created_at, id`, transferTSFormat, srcCol, s.unitsTable())
	}
	rows, err := s.pool.Query(ctx, query, bankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.TransferObservation
	for rows.Next() {
		var o model.TransferObservation
		var tags, sources []string
		if err := rows.Scan(&o.SourceID, &o.Text, &tags, &o.MentionedAt, &o.CreatedAt, &sources); err != nil {
			return nil, err
		}
		o.Tags = tags
		o.Sources = sources
		if len(sources) > 0 {
			o.ProofCount = len(sources)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RestoreTransferDocument writes one archive document into a bank. Passing
// through Retain keeps the same document/unit/embedding path a normal retain
// takes, so an import never invents facts and never calls the LLM.
func (s *Store) RestoreTransferDocument(ctx context.Context, bankID string, doc model.TransferDocument, onConflict string) (model.TransferOutcome, error) {
	if b, err := s.GetBank(ctx, bankID); err != nil {
		return model.TransferOutcome{}, err
	} else if b == nil {
		return model.TransferOutcome{}, ErrBankNotFound
	}

	exists := false
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM documents WHERE bank_id = $1 AND id = $2`, bankID, doc.ID).Scan(new(int))
	switch {
	case err == nil:
		exists = true
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return model.TransferOutcome{}, err
	}

	targetID := doc.ID
	switch {
	case !exists:
	case onConflict == "skip":
		return model.TransferOutcome{Skipped: true}, nil
	case onConflict == "new-id":
		targetID = doc.ID + "-" + model.ContentHash(doc.ID + bankID)[:8]
	case onConflict == "replace":
		if err := s.deleteDocumentRows(ctx, bankID, doc.ID); err != nil {
			return model.TransferOutcome{}, err
		}
	default:
		return model.TransferOutcome{}, fmt.Errorf("invalid on_conflict %q", onConflict)
	}

	items := transferItems(doc, targetID)
	if len(items) == 0 {
		// An empty document still exists as a row so a later retain can attach
		// to it; upstream preserves documents that carried no extracted facts.
		if err := s.insertEmptyDocument(ctx, bankID, targetID, doc); err != nil {
			return model.TransferOutcome{}, err
		}
		return model.TransferOutcome{}, nil
	}
	res, err := s.Retain(ctx, bankID, items)
	if err != nil {
		return model.TransferOutcome{}, err
	}
	return model.TransferOutcome{CreatedFacts: res.UnitsStored}, nil
}

func (s *Store) deleteDocumentRows(ctx context.Context, bankID, docID string) error {
	if _, err := s.pool.Exec(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE bank_id = $1 AND document_id = $2", s.unitsTable()), bankID, docID); err != nil {
		return err
	}
	if s.upstream {
		if _, err := s.pool.Exec(ctx, `DELETE FROM chunks WHERE bank_id = $1 AND document_id = $2`, bankID, docID); err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM documents WHERE bank_id = $1 AND id = $2`, bankID, docID)
	return err
}

func (s *Store) insertEmptyDocument(ctx context.Context, bankID, docID string, doc model.TransferDocument) error {
	text := doc.OriginalText
	hash := model.ContentHash(text)
	_, err := s.pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO documents (id, bank_id, original_text, content_hash, tags) VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (bank_id, id) DO UPDATE SET original_text = EXCLUDED.original_text, content_hash = EXCLUDED.content_hash, tags = EXCLUDED.tags%s`,
		s.docTouchSet()), docID, bankID, text, hash, normalizeTags(doc.Tags))
	return err
}

// RestoreTransferObservation inserts a consolidated observation. The unit
// keeps its own text and tags; the facts it cites are not re-linked, because an
// archive's source ids do not survive the transfer.
func (s *Store) RestoreTransferObservation(ctx context.Context, bankID string, obs model.TransferObservation) error {
	if strings.TrimSpace(obs.Text) == "" {
		return nil
	}
	id := model.UnitID(bankID, "observation", obs.Text)
	columns := "id, bank_id, document_id, fact_type, text, context, chunk_id, tags"
	values := fmt.Sprintf("$1,$2,%s,'observation',$3,'',%s,$4", s.observationDocumentID(), s.observationChunkID())
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (id) DO NOTHING",
		s.unitsTable(), columns, values), id, bankID, obs.Text, normalizeTags(obs.Tags)); err != nil {
		return err
	}
	return nil
}

// transferItems converts an archive document into retain items. Extracted
// facts win when present; a chunks-only archive (verbatim retention) falls back
// to its chunks so no text is dropped.
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

// BankAttachment resolves one inline attachment's storage key and media type.
// Only the upstream schema keeps an attachments table; the lite schema has
// none, so it answers "not found" rather than inventing one.
func (s *Store) BankAttachment(ctx context.Context, bankID, attachmentID string) (string, string, error) {
	if !s.upstream {
		return "", "", nil
	}
	var key, mediaType string
	err := s.pool.QueryRow(ctx, `
SELECT COALESCE(storage_key,''), COALESCE(media_type,'')
FROM attachments
WHERE bank_id = $1 AND short_id = $2
ORDER BY created_at, attachment_hash
LIMIT 1`, bankID, attachmentID).Scan(&key, &mediaType)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		// A schema predating the attachments table answers "not found"
		// instead of failing the read.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return "", "", nil
		}
		return "", "", err
	}
	return key, mediaType, nil
}

// UpdateDirective rewrites the content of the directive with this name,
// creating it when the bank has none. Template import matches by name.
func (s *Store) UpdateDirective(ctx context.Context, bankID, name, content string) error {
	if err := s.ensureDirectivesTable(ctx); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE directives SET content = $3 WHERE bank_id = $1 AND name = $2`, bankID, name, content)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		_, err := s.CreateDirective(ctx, bankID, name, content)
		return err
	}
	return nil
}
