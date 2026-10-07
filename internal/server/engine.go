package server

import (
	"context"
	"errors"
	"fmt"
	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"net/http"
	"strings"
	"time"

	"github.com/EvolveHsu/hindsight-go/internal/consolidate"
	"github.com/EvolveHsu/hindsight-go/internal/extract"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Engine wires the generated HTTP surface to a Store.
type Engine struct {
	// Server supplies the monitoring surface (health/live/ready/version/metrics).
	Server
	store          Store
	extractor      *extract.Extractor
	provider       reflectProvider
	consolProvider consolidate.Provider
	files          *FileStore
}

// NewEngine binds a storage backend.
func NewEngine(s Store) *Engine { return &Engine{store: s} }

// SetExtractor wires the LLM fact extractor used by retain.
func (e *Engine) SetExtractor(x *extract.Extractor) { e.extractor = x }

// NewEngineWithFiles binds a storage backend and the blob store the transfer
// endpoints write archives to and read attachments from.
func NewEngineWithFiles(s Store, files *FileStore) *Engine {
	return &Engine{store: s, files: files}
}

func notFound(format string, a ...any) error {
	return &statusError{status: http.StatusNotFound, body: map[string]any{"detail": fmt.Sprintf(format, a...)}}
}

func badRequest(format string, a ...any) error {
	return &statusError{status: http.StatusBadRequest, body: map[string]any{"detail": fmt.Sprintf(format, a...)}}
}

func internal(err error) error {
	return &statusError{status: http.StatusInternalServerError, body: map[string]any{"detail": err.Error()}}
}

// mapStoreErr maps seam errors onto HTTP responses.
func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, model.ErrBankNotFound), errors.Is(err, memoryErrBankNotFound):
		return notFound("bank does not exist")
	default:
		return internal(err)
	}
}

// memoryErrBankNotFound lets the in-memory backend error (defined in
// internal/memory) participate in the same mapping without importing it here.
var memoryErrBankNotFound = errors.New("bank not found")

// CreateOrUpdateBank implements create_or_update_bank (PUT /banks/{bank_id}).
func (e *Engine) CreateOrUpdateBank(ctx context.Context, req *api.CreateBankRequest, params api.CreateOrUpdateBankParams) (api.CreateOrUpdateBankRes, error) {
	if params.BankID == "" {
		return nil, badRequest("bank_id is required")
	}
	// PUT is PATCH plus an implicit create: a missing bank is created first,
	// and an existing bank keeps the fields the request did not mention.
	if _, err := e.store.EnsureBank(ctx, params.BankID, "", ""); err != nil {
		return nil, internal(err)
	}
	if _, ok := e.store.(BankAdminSource); !ok {
		b, err := e.store.GetBank(ctx, params.BankID)
		if err != nil {
			return nil, internal(err)
		}
		if b == nil {
			return nil, notFound("bank %q does not exist", params.BankID)
		}
		return e.bankProfileResponse(ctx, b)
	}
	profile, err := e.applyBankPatch(ctx, params.BankID, req)
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// RetainMemories implements retain_memories (POST /banks/{bank_id}/memories).
func (e *Engine) RetainMemories(ctx context.Context, req *api.RetainRequest, params api.RetainMemoriesParams) (api.RetainMemoriesRes, error) {
	if req == nil || len(req.Items) == 0 {
		return nil, badRequest("items must contain at least one memory item")
	}
	if params.BankID == "" {
		return nil, badRequest("bank_id is required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}

	items := make([]model.RetainItem, 0, len(req.Items))
	for i, it := range req.Items {
		text, err := contentText(it.Content)
		if err != nil {
			return nil, badRequest("items[%d].content: %v", i, err)
		}
		if strings.TrimSpace(text) == "" {
			return nil, badRequest("items[%d].content is empty", i)
		}
		ri := model.RetainItem{
			Content:    text,
			DocumentID: it.DocumentID.Value,
			Context:    it.Context.Value,
			Tags:       it.Tags,
			FactType:   model.FactExperience, // lite default for conversation content
		}
		if it.Timestamp.Set {
			ri.MentionedAt = it.Timestamp.Value.UTC().Format(time.RFC3339)
		}
		items = append(items, ri)
	}
	storedItems := items
	if e.extractor != nil {
		extracted := make([]model.RetainItem, 0, len(items))
		for _, ri := range items {
			// Explicit verbatim strategies opt out of the extractor, matching upstream's strategy seam.
			facts, xerr := e.extractor.Extract(ctx, ri.Content, ri.Context)
			if xerr != nil {
				extracted = append(extracted, ri)
				continue
			}
			docID := ri.DocumentID
			if docID == "" {
				docID = "auto-" + model.ContentHash(ri.Content)[:12]
			}
			for _, fact := range facts {
				factType := model.FactWorld
				if fact.FactType == "experience" {
					factType = model.FactExperience
				}
				extracted = append(extracted, model.RetainItem{
					Content:     fact.Text,
					DocumentID:  docID,
					Tags:        ri.Tags,
					Entities:    fact.Entities,
					Context:     ri.Context,
					MentionedAt: ri.MentionedAt,
					FactType:    factType,
				})
			}
		}
		if len(extracted) > 0 {
			storedItems = extracted
		}
	}

	res, err := e.store.Retain(ctx, params.BankID, storedItems)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	// Async retain keeps its contract - an operation id to poll - without a
	// background queue: the work runs inline and the finished operation is
	// recorded, so a caller polls the id and sees it completed.
	if req.Async.Set && req.Async.Value {
		opID := recordOperation(params.BankID, "retain", map[string]any{
			"items_count":  res.ItemsCount,
			"document_ids": res.DocumentIDs,
		})
		return &api.RetainResponse{
			Success:     true,
			BankID:      res.BankID,
			ItemsCount:  res.ItemsCount,
			Async:       true,
			OperationID: api.OptString{Set: true, Value: opID},
		}, nil
	}
	return &api.RetainResponse{
		Success:    true,
		BankID:     res.BankID,
		ItemsCount: res.ItemsCount,
		Async:      false,
	}, nil
}

// RecallMemories implements recall_memories (POST /banks/{bank_id}/memories/recall).
func (e *Engine) RecallMemories(ctx context.Context, req *api.RecallRequest, params api.RecallMemoriesParams) (api.RecallMemoriesRes, error) {
	if req == nil || strings.TrimSpace(req.Query) == "" {
		return nil, badRequest("query is required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}

	opt := model.RecallOptions{Query: req.Query}
	if req.Budget.Set {
		switch req.Budget.Value {
		case api.BudgetLow:
			opt.Budget = 20
		case api.BudgetMid:
			opt.Budget = 40
		case api.BudgetHigh:
			opt.Budget = 80
		}
	}
	for _, t := range req.Types {
		switch t {
		case "world":
			opt.Types = append(opt.Types, model.FactWorld)
		case "experience":
			opt.Types = append(opt.Types, model.FactExperience)
		case "observation":
			opt.Types = append(opt.Types, model.FactObservation)
		default:
			return nil, badRequest("unknown fact type %q", t)
		}
	}
	if req.MinScores.Set {
		if req.MinScores.Value.Semantic.Set {
			opt.MinSemantic = req.MinScores.Value.Semantic.Value
			opt.MinSemanticSet = true
		}
		if req.MinScores.Value.Keyword.Set {
			opt.MinKeyword = req.MinScores.Value.Keyword.Value
			opt.MinKeywordSet = true
		}
	}

	hits, err := e.store.Recall(ctx, params.BankID, opt)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	hits, err = applyRecallRequestFilters(hits, req)
	if err != nil {
		return nil, err
	}

	out := make([]api.RecallResult, 0, len(hits))
	for _, h := range hits {
		u := h.Unit
		out = append(out, api.RecallResult{
			ID:          u.ID,
			Text:        u.Text,
			Entities:    u.Entities,
			Context:     optString(u.Context),
			DocumentID:  optString(u.DocumentID),
			ChunkID:     optString(u.ChunkID),
			Tags:        u.Tags,
			MentionedAt: optString(u.MentionedAt),
			Scores: api.OptRecallScores{
				Set: true,
				Value: api.RecallScores{
					Final:    h.FusedScore,
					Semantic: optFloat(h.Semantic),
					Keyword:  optFloat(h.Keyword),
				},
			},
		})
	}
	return &api.RecallResponse{Results: out}, nil
}

// ListTags implements list_tags (GET /banks/{bank_id}/tags).
func (e *Engine) ListTags(ctx context.Context, params api.ListTagsParams) (api.ListTagsRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	counts, err := e.store.ListTags(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	limit, offset := params.Limit.Or(50), params.Offset.Or(0)
	items := make([]api.TagItem, 0, len(counts))
	for tag, n := range counts {
		if params.Q.Set && params.Q.Value != "" &&
			!strings.Contains(strings.ToLower(tag), strings.ToLower(params.Q.Value)) {
			continue
		}
		items = append(items, api.TagItem{Tag: tag, Count: n})
	}
	sortTagItems(items)
	total := len(items)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	return &api.ListTagsResponse{Items: items[offset:end], Total: total, Limit: limit, Offset: offset}, nil
}

// ListBanks implements list_banks (GET /banks).
func (e *Engine) ListBanks(ctx context.Context, params api.ListBanksParams) (api.ListBanksRes, error) {
	ids, err := e.store.BankIDs(ctx)
	if err != nil {
		return nil, internal(err)
	}
	items := make([]api.BankListItem, 0, len(ids))
	for _, id := range ids {
		b, err := e.store.GetBank(ctx, id)
		if err != nil {
			return nil, internal(err)
		}
		if b == nil {
			continue
		}
		if params.Q.Set && params.Q.Value != "" &&
			!strings.Contains(strings.ToLower(b.ID), strings.ToLower(params.Q.Value)) &&
			!strings.Contains(strings.ToLower(b.Name), strings.ToLower(params.Q.Value)) {
			continue
		}
		profile, _ := e.bankProfileResponse(ctx, b)
		item := api.BankListItem{BankID: b.ID, Name: optString(profile.Name), Mission: optString(profile.Mission)}
		if src, ok := e.store.(statsOpsSource); ok {
			if stats, err := src.BankStats(ctx, b.ID); err == nil {
				item.FactCount = api.OptInt{Set: true, Value: stats.TotalNodes}
				if stats.CreatedAt != "" {
					item.CreatedAt = optString(stats.CreatedAt)
				}
				if stats.UpdatedAt != "" {
					item.UpdatedAt = optString(stats.UpdatedAt)
				}
				if stats.LastDocumentAt != "" {
					item.LastDocumentAt = optString(stats.LastDocumentAt)
				}
				if stats.LastMemoryWriteAt != "" {
					item.LastWriteAt = optString(stats.LastMemoryWriteAt)
				}
			}
		}
		items = append(items, item)
	}
	limit, offset := params.Limit.Or(50), params.Offset.Or(0)
	total := len(items)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	return &api.BankListResponse{Banks: items[offset:end], Total: total, Limit: limit, Offset: offset}, nil
}

// contentText unwraps retain content blocks into text. The plain-string form is
// handled upstream by the content bridge (content_bridge.go).
func contentText(c api.Content) (string, error) {
	var parts []string
	for _, blk := range c {
		if blk.Type == api.TextContentBlockContentItem {
			parts = append(parts, blk.TextContentBlock.Text)
		}
	}
	switch len(parts) {
	case 0:
		return "", errors.New("no text content")
	case 1:
		return parts[0], nil
	default:
		return strings.Join(parts, "\n"), nil
	}
}

// parseRFC3339 accepts the ISO-8601 forms the upstream API documents.
func parseRFC3339(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if v, err := time.Parse(layout, s); err == nil {
			return v, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse timestamp %q", s)
}

func sortTagItems(items []api.TagItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			a, b := items[j-1], items[j]
			if a.Count < b.Count || (a.Count == b.Count && a.Tag > b.Tag) {
				items[j-1], items[j] = b, a
			} else {
				break
			}
		}
	}
}

func optString(s string) api.OptString {
	if s == "" {
		return api.OptString{}
	}
	return api.OptString{Set: true, Value: s}
}

func optFloat(f float64) api.OptFloat64 {
	if f == 0 {
		return api.OptFloat64{}
	}
	return api.OptFloat64{Set: true, Value: f}
}
