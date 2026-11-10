package server

import (
	"context"
	"fmt"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
)

// The v1.2.1 /pages routes are the canonical names for the existing
// knowledge-base implementation. Keep one storage implementation and expose
// both wire contracts so the deprecated /knowledge-base aliases remain
// byte-compatible.

// GetPageTree implements get_page_tree.
func (e *Engine) GetPageTree(ctx context.Context, params api.GetPageTreeParams) (api.GetPageTreeRes, error) {
	legacy := api.GetKnowledgeBaseTreeParams{
		BankID:    params.BankID,
		Tags:      params.Tags,
		TagGroups: params.TagGroups,
	}
	if params.TagsMatch.Set {
		legacy.TagsMatch = api.OptGetKnowledgeBaseTreeTagsMatch{
			Set:   true,
			Value: api.GetKnowledgeBaseTreeTagsMatch(params.TagsMatch.Value),
		}
	}
	res, err := e.GetKnowledgeBaseTree(ctx, legacy)
	if err != nil {
		return nil, err
	}
	tree, ok := res.(*api.KnowledgeTreeResponse)
	if !ok {
		return nil, internal(fmt.Errorf("unexpected knowledge tree response %T", res))
	}
	return pageTreeFromKnowledge(tree), nil
}

// GetPage implements get_page.
func (e *Engine) GetPage(ctx context.Context, params api.GetPageParams) (api.GetPageRes, error) {
	res, err := e.GetKnowledgePage(ctx, api.GetKnowledgePageParams{
		BankID: params.BankID,
		PageID: params.PageID,
	})
	if err != nil {
		return nil, err
	}
	page, ok := res.(*api.KnowledgePageResponse)
	if !ok {
		return nil, internal(fmt.Errorf("unexpected knowledge page response %T", res))
	}
	return pageFromKnowledge(page), nil
}

// SearchPages implements search_pages.
func (e *Engine) SearchPages(ctx context.Context, params api.SearchPagesParams) (api.SearchPagesRes, error) {
	legacy := api.SearchKnowledgeBaseParams{
		BankID:    params.BankID,
		Q:         params.Q,
		Limit:     params.Limit,
		Tags:      params.Tags,
		TagGroups: params.TagGroups,
	}
	if params.TagsMatch.Set {
		legacy.TagsMatch = api.OptSearchKnowledgeBaseTagsMatch{
			Set:   true,
			Value: api.SearchKnowledgeBaseTagsMatch(params.TagsMatch.Value),
		}
	}
	res, err := e.SearchKnowledgeBase(ctx, legacy)
	if err != nil {
		return nil, err
	}
	search, ok := res.(*api.KnowledgePageSearchResponse)
	if !ok {
		return nil, internal(fmt.Errorf("unexpected page search response %T", res))
	}
	return pageSearchFromKnowledge(search), nil
}

// CreatePage implements create_page.
func (e *Engine) CreatePage(ctx context.Context, req *api.CreatePageRequest, params api.CreatePageParams) (api.CreatePageRes, error) {
	res, err := e.CreateKnowledgePage(ctx, req, api.CreateKnowledgePageParams{BankID: params.BankID})
	if err != nil {
		return nil, err
	}
	created, ok := res.(*api.CreateKnowledgePageResponse)
	if !ok {
		return nil, internal(fmt.Errorf("unexpected create page response %T", res))
	}
	return &api.CreatePageResponse{
		PageID:        created.PageID,
		MentalModelID: created.MentalModelID,
		OperationID:   created.OperationID,
	}, nil
}

// CreatePageFolder implements create_page_folder.
func (e *Engine) CreatePageFolder(ctx context.Context, req *api.CreateFolderRequest, params api.CreatePageFolderParams) (api.CreatePageFolderRes, error) {
	res, err := e.CreateKnowledgeFolder(ctx, req, api.CreateKnowledgeFolderParams{BankID: params.BankID})
	if err != nil {
		return nil, err
	}
	node, ok := res.(*api.KnowledgeNode)
	if !ok {
		return nil, internal(fmt.Errorf("unexpected create page folder response %T", res))
	}
	out := pageNodeFromKnowledge(*node)
	return &out, nil
}

// UpdatePageNode implements update_page_node.
func (e *Engine) UpdatePageNode(ctx context.Context, req *api.UpdateNodeRequest, params api.UpdatePageNodeParams) (api.UpdatePageNodeRes, error) {
	res, err := e.UpdateKnowledgeNode(ctx, req, api.UpdateKnowledgeNodeParams{
		BankID: params.BankID,
		NodeID: params.NodeID,
	})
	if err != nil {
		return nil, err
	}
	node, ok := res.(*api.KnowledgeNode)
	if !ok {
		return nil, internal(fmt.Errorf("unexpected update page node response %T", res))
	}
	out := pageNodeFromKnowledge(*node)
	return &out, nil
}

// DeletePageNode implements delete_page_node.
func (e *Engine) DeletePageNode(ctx context.Context, params api.DeletePageNodeParams) (api.DeletePageNodeRes, error) {
	res, err := e.DeleteKnowledgeNode(ctx, api.DeleteKnowledgeNodeParams{
		BankID: params.BankID,
		NodeID: params.NodeID,
	})
	if err != nil {
		return nil, err
	}
	deleted, ok := res.(*api.DeleteKnowledgeNodeOKApplicationJSON)
	if !ok {
		return nil, internal(fmt.Errorf("unexpected delete page node response %T", res))
	}
	out := api.DeletePageNodeOKApplicationJSON(*deleted)
	return &out, nil
}

// ExportPages implements export_pages.
func (e *Engine) ExportPages(ctx context.Context, params api.ExportPagesParams) (api.ExportPagesRes, error) {
	res, err := e.ExportKnowledgeBase(ctx, api.ExportKnowledgeBaseParams{BankID: params.BankID})
	if err != nil {
		return nil, err
	}
	bundle, ok := res.(*api.KnowledgePageBundleResponse)
	if !ok {
		return nil, internal(fmt.Errorf("unexpected page export response %T", res))
	}
	return pageBundleFromKnowledge(bundle), nil
}

func pageTreeFromKnowledge(in *api.KnowledgeTreeResponse) *api.PageTreeResponse {
	roots := make([]api.PageNode, 0, len(in.Roots))
	for _, node := range in.Roots {
		roots = append(roots, pageNodeFromKnowledge(node))
	}
	return &api.PageTreeResponse{Roots: roots}
}

func pageNodeFromKnowledge(in api.KnowledgeNode) api.PageNode {
	children := make([]api.PageNode, 0, len(in.Children))
	for _, child := range in.Children {
		children = append(children, pageNodeFromKnowledge(child))
	}
	kind := api.PageNodeKindFolder
	if in.Kind == api.KnowledgeNodeKindPage {
		kind = api.PageNodeKindPage
	}
	return api.PageNode{
		ID:                  in.ID,
		Kind:                kind,
		Name:                in.Name,
		ParentID:            in.ParentID,
		MentalModelID:       in.MentalModelID,
		Managed:             in.Managed,
		Description:         in.Description,
		Tags:                in.Tags,
		Timestamp:           in.Timestamp,
		IsStale:             in.IsStale,
		LastRefreshFailedAt: in.LastRefreshFailedAt,
		Trigger:             in.Trigger,
		Children:            children,
	}
}

func pageFromKnowledge(in *api.KnowledgePageResponse) *api.PageResponse {
	return &api.PageResponse{
		ID:          in.ID,
		Name:        in.Name,
		Type:        in.Type,
		Description: in.Description,
		Tags:        in.Tags,
		Timestamp:   in.Timestamp,
		Body:        in.Body,
		Markdown:    in.Markdown,
	}
}

func pageSearchFromKnowledge(in *api.KnowledgePageSearchResponse) *api.PageSearchResponse {
	results := make([]api.PageSearchResult, 0, len(in.Results))
	for _, result := range in.Results {
		results = append(results, api.PageSearchResult{
			ID:            result.ID,
			Name:          result.Name,
			MentalModelID: result.MentalModelID,
			SourceQuery:   result.SourceQuery,
			Snippet:       result.Snippet,
			Score:         result.Score,
			UpdatedAt:     result.UpdatedAt,
		})
	}
	return &api.PageSearchResponse{Results: results, Total: in.Total}
}

func pageBundleFromKnowledge(in *api.KnowledgePageBundleResponse) *api.PageBundleResponse {
	files := make([]api.PageBundleFile, 0, len(in.Files))
	for _, file := range in.Files {
		files = append(files, api.PageBundleFile{Path: file.Path, Content: file.Content})
	}
	return &api.PageBundleResponse{Files: files}
}
