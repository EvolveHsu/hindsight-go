package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// KnowledgeSource is the persistence slice used by the knowledge-base API.
// It is implemented by the PostgreSQL backend; the in-memory backend answers
// with a clear 501 rather than pretending the feature exists.
type KnowledgeSource interface {
	ListKnowledgeRows(ctx context.Context, bankID string) ([]model.KnowledgeRow, error)
	GetKnowledgeRow(ctx context.Context, bankID, id string) (*model.KnowledgeRow, error)
	CreateKnowledgeRow(ctx context.Context, bankID string, row model.KnowledgeRow) error
	UpdateKnowledgeRow(ctx context.Context, bankID, id string, patch model.KnowledgePatch) error
	DeleteKnowledgeSubtree(ctx context.Context, bankID, id string) ([]string, error)
	SearchKnowledgeRows(ctx context.Context, bankID, query string, limit int) ([]model.KnowledgeRow, error)
}

func (e *Engine) knowledgeSource() (KnowledgeSource, error) {
	if ks, ok := e.store.(KnowledgeSource); ok {
		return ks, nil
	}
	return nil, &statusError{status: http.StatusNotImplemented, body: map[string]any{"detail": "backend does not support the knowledge base"}}
}

func parityID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "0000000000000000"
	}
	return prefix + hex.EncodeToString(b[:])
}

func (e *Engine) GetKnowledgeBaseTree(ctx context.Context, params api.GetKnowledgeBaseTreeParams) (api.GetKnowledgeBaseTreeRes, error) {
	ks, err := e.knowledgeSource()
	if err != nil {
		return nil, err
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	rows, err := ks.ListKnowledgeRows(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	if len(params.Tags) > 0 {
		filtered := rows[:0]
		for _, r := range rows {
			if knowledgeTagsMatch(r.Tags, params.Tags, params.TagsMatch) {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	nodes := make(map[string]*api.KnowledgeNode, len(rows))
	for _, r := range rows {
		node := knowledgeNode(r)
		nodes[r.ID] = &node
	}
	roots := make([]api.KnowledgeNode, 0)
	for _, r := range rows {
		node := nodes[r.ID]
		if r.ParentID == "" {
			roots = append(roots, *node)
			continue
		}
		if parent := nodes[r.ParentID]; parent != nil {
			parent.Children = append(parent.Children, *node)
			continue
		}
		// Orphaned nodes are still returned, otherwise they disappear from the UI.
		roots = append(roots, *node)
	}
	return &api.KnowledgeTreeResponse{Roots: roots}, nil
}

func knowledgeNode(r model.KnowledgeRow) api.KnowledgeNode {
	kind := api.KnowledgeNodeKindFolder
	if r.Kind == "page" {
		kind = api.KnowledgeNodeKindPage
	}
	node := api.KnowledgeNode{
		ID:                  r.ID,
		Kind:                kind,
		Name:                r.Name,
		Managed:             api.OptBool{Set: true, Value: r.Managed},
		Tags:                r.Tags,
		Children:            []api.KnowledgeNode{},
		Description:         optString(r.Description),
		Timestamp:           optString(r.UpdatedAt),
		IsStale:             api.OptBool{Set: true, Value: false},
		LastRefreshFailedAt: optString(r.LastRefreshFailedAt),
	}
	if r.ParentID != "" {
		node.ParentID = optString(r.ParentID)
	}
	if r.MentalModelID != "" {
		node.MentalModelID = optString(r.MentalModelID)
	}
	return node
}

func knowledgeTagsMatch(pageTags, wanted []string, match api.OptGetKnowledgeBaseTreeTagsMatch) bool {
	if len(wanted) == 0 {
		return true
	}
	has := func(tag string) bool {
		for _, t := range pageTags {
			if t == tag {
				return true
			}
		}
		return false
	}
	mode := ""
	if match.Set {
		mode = string(match.Value)
	}
	switch mode {
	case "all", "all_strict":
		for _, tag := range wanted {
			if !has(tag) {
				return false
			}
		}
		return true
	case "exact":
		if len(pageTags) != len(wanted) {
			return false
		}
		for _, tag := range wanted {
			if !has(tag) {
				return false
			}
		}
		return true
	default:
		for _, tag := range wanted {
			if has(tag) {
				return true
			}
		}
		return false
	}
}

func (e *Engine) GetKnowledgePage(ctx context.Context, params api.GetKnowledgePageParams) (api.GetKnowledgePageRes, error) {
	ks, err := e.knowledgeSource()
	if err != nil {
		return nil, err
	}
	row, err := ks.GetKnowledgeRow(ctx, params.BankID, params.PageID)
	if err != nil {
		return nil, internal(err)
	}
	if row == nil || row.Kind != "page" {
		return nil, notFound("knowledge page %q not found", params.PageID)
	}
	body := row.Body
	markdown := renderKnowledgePage(*row)
	return &api.KnowledgePageResponse{
		ID:          row.ID,
		Name:        row.Name,
		Type:        knowledgePageType(row.Tags),
		Description: optString(row.Description),
		Tags:        row.Tags,
		Timestamp:   optString(row.LastRefreshedAt),
		Body:        optString(body),
		Markdown:    markdown,
	}, nil
}

func knowledgePageType(tags []string) string {
	for _, tag := range tags {
		if strings.HasPrefix(tag, "type:") && len(tag) > len("type:") {
			return strings.TrimPrefix(tag, "type:")
		}
	}
	return "knowledge-page"
}

func renderKnowledgePage(r model.KnowledgeRow) string {
	tags, _ := json.Marshal(r.Tags)
	body := r.Body
	if strings.TrimSpace(body) == "" {
		body = "No content yet."
	}
	return fmt.Sprintf("---\nid: %s\nname: %s\ntype: %s\ntags: %s\n---\n\n%s",
		r.ID, r.Name, knowledgePageType(r.Tags), string(tags), body)
}

func (e *Engine) SearchKnowledgeBase(ctx context.Context, params api.SearchKnowledgeBaseParams) (api.SearchKnowledgeBaseRes, error) {
	ks, err := e.knowledgeSource()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.Q) == "" {
		return &api.KnowledgePageSearchResponse{Results: []api.KnowledgePageSearchResult{}, Total: 0}, nil
	}
	limit := params.Limit.Or(50)
	rows, err := ks.SearchKnowledgeRows(ctx, params.BankID, params.Q, limit)
	if err != nil {
		return nil, internal(err)
	}
	results := make([]api.KnowledgePageSearchResult, 0, len(rows))
	for _, r := range rows {
		snippet := strings.TrimSpace(r.Body)
		if len(snippet) > 240 {
			snippet = snippet[:240]
		}
		if snippet == "" {
			snippet = "No content yet."
		}
		results = append(results, api.KnowledgePageSearchResult{
			ID:            r.ID,
			Name:          r.Name,
			MentalModelID: optString(r.MentalModelID),
			SourceQuery:   optString(r.Description),
			Snippet:       snippet,
			Score:         1,
			UpdatedAt:     optString(r.UpdatedAt),
		})
	}
	return &api.KnowledgePageSearchResponse{Results: results, Total: len(results)}, nil
}

func (e *Engine) CreateKnowledgeFolder(ctx context.Context, req *api.CreateFolderRequest, params api.CreateKnowledgeFolderParams) (api.CreateKnowledgeFolderRes, error) {
	ks, err := e.knowledgeSource()
	if err != nil {
		return nil, err
	}
	if req == nil || strings.TrimSpace(req.Name) == "" {
		return nil, badRequest("name is required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	row := model.KnowledgeRow{ID: parityID("kf-"), BankID: params.BankID, Kind: "folder", Name: req.Name, Managed: false}
	if req.ParentID.Set {
		row.ParentID = req.ParentID.Value
	}
	if err := ks.CreateKnowledgeRow(ctx, params.BankID, row); err != nil {
		return nil, internal(err)
	}
	return &api.KnowledgeNode{ID: row.ID, Kind: api.KnowledgeNodeKindFolder, Name: row.Name, Children: []api.KnowledgeNode{}}, nil
}

func (e *Engine) CreateKnowledgePage(ctx context.Context, req *api.CreatePageRequest, params api.CreateKnowledgePageParams) (api.CreateKnowledgePageRes, error) {
	ks, err := e.knowledgeSource()
	if err != nil {
		return nil, err
	}
	if req == nil || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.SourceQuery) == "" {
		return nil, badRequest("name and source_query are required")
	}
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	mms, err := e.mmStore()
	if err != nil {
		return nil, err
	}
	pageID := parityID("kp-")
	modelID := parityID("mm-")
	maxTokens := 0
	if req.MaxTokens.Set {
		maxTokens = req.MaxTokens.Value
	}
	m := &model.MentalModel{
		ID: modelID, Name: req.Name, SourceQuery: req.SourceQuery,
		Tags: normalizeTags(req.Tags), MaxTokens: maxTokens,
	}
	if _, err := mms.CreateMentalModel(ctx, params.BankID, m); err != nil {
		return nil, internal(err)
	}
	row := model.KnowledgeRow{ID: pageID, BankID: params.BankID, Kind: "page", Name: req.Name, MentalModelID: modelID}
	if req.ParentID.Set {
		row.ParentID = req.ParentID.Value
	}
	if err := ks.CreateKnowledgeRow(ctx, params.BankID, row); err != nil {
		return nil, internal(err)
	}
	return &api.CreateKnowledgePageResponse{PageID: pageID, MentalModelID: modelID, OperationID: optString("mm-refresh-" + modelID)}, nil
}

func (e *Engine) UpdateKnowledgeNode(ctx context.Context, req *api.UpdateNodeRequest, params api.UpdateKnowledgeNodeParams) (api.UpdateKnowledgeNodeRes, error) {
	ks, err := e.knowledgeSource()
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, badRequest("request is required")
	}
	patch := model.KnowledgePatch{}
	if req.Name.Set {
		patch.Name = &req.Name.Value
	}
	if req.ParentID.Set {
		patch.ParentID = &req.ParentID.Value
	}
	if req.SourceQuery.Set {
		patch.SourceQuery = &req.SourceQuery.Value
	}
	if req.Tags != nil {
		patch.Tags = &req.Tags
	}
	if req.MaxTokens.Set {
		patch.MaxTokens = &req.MaxTokens.Value
	}
	if req.Trigger.Set {
		raw, err := json.Marshal(req.Trigger.Value)
		if err != nil {
			return nil, badRequest("invalid trigger: %v", err)
		}
		patch.Trigger = raw
	}
	if err := ks.UpdateKnowledgeRow(ctx, params.BankID, params.NodeID, patch); err != nil {
		return nil, internal(err)
	}
	row, err := ks.GetKnowledgeRow(ctx, params.BankID, params.NodeID)
	if err != nil {
		return nil, internal(err)
	}
	if row == nil {
		return nil, notFound("knowledge node %q not found", params.NodeID)
	}
	n := knowledgeNode(*row)
	return &n, nil
}

func (e *Engine) DeleteKnowledgeNode(ctx context.Context, params api.DeleteKnowledgeNodeParams) (api.DeleteKnowledgeNodeRes, error) {
	ks, err := e.knowledgeSource()
	if err != nil {
		return nil, err
	}
	ids, err := ks.DeleteKnowledgeSubtree(ctx, params.BankID, params.NodeID)
	if err != nil {
		return nil, internal(err)
	}
	if len(ids) == 0 {
		return nil, notFound("knowledge node %q not found", params.NodeID)
	}
	raw, _ := json.Marshal(map[string]any{"success": true, "deleted_count": len(ids)})
	return (*api.DeleteKnowledgeNodeOKApplicationJSON)(&raw), nil
}

func (e *Engine) ExportKnowledgeBase(ctx context.Context, params api.ExportKnowledgeBaseParams) (api.ExportKnowledgeBaseRes, error) {
	ks, err := e.knowledgeSource()
	if err != nil {
		return nil, err
	}
	rows, err := ks.ListKnowledgeRows(ctx, params.BankID)
	if err != nil {
		return nil, internal(err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	files := make([]api.KnowledgePageBundleFile, 0, len(rows)+1)
	index := strings.Builder{}
	for _, r := range rows {
		if r.Kind != "page" {
			continue
		}
		index.WriteString("* [")
		index.WriteString(r.Name)
		index.WriteString("](")
		index.WriteString(r.ID)
		index.WriteString(".md)\n")
		files = append(files, api.KnowledgePageBundleFile{Path: r.ID + ".md", Content: renderKnowledgePage(r)})
	}
	files = append([]api.KnowledgePageBundleFile{{Path: "index.md", Content: index.String()}}, files...)
	return &api.KnowledgePageBundleResponse{Files: files}, nil
}
