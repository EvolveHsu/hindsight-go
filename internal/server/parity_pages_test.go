package server

import (
	"context"
	"net/http"
	"testing"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/memory"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

type pageTestStore struct {
	*memory.Store
	rows []model.KnowledgeRow
}

func (s *pageTestStore) ListKnowledgeRows(context.Context, string) ([]model.KnowledgeRow, error) {
	return append([]model.KnowledgeRow(nil), s.rows...), nil
}

func (s *pageTestStore) GetKnowledgeRow(_ context.Context, _ string, id string) (*model.KnowledgeRow, error) {
	for i := range s.rows {
		if s.rows[i].ID == id {
			row := s.rows[i]
			return &row, nil
		}
	}
	return nil, nil
}

func (s *pageTestStore) CreateKnowledgeRow(_ context.Context, _ string, row model.KnowledgeRow) error {
	s.rows = append(s.rows, row)
	return nil
}

func (s *pageTestStore) UpdateKnowledgeRow(context.Context, string, string, model.KnowledgePatch) error {
	return nil
}

func (s *pageTestStore) DeleteKnowledgeSubtree(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (s *pageTestStore) SearchKnowledgeRows(_ context.Context, _ string, _ string, limit int) ([]model.KnowledgeRow, error) {
	if limit > len(s.rows) {
		limit = len(s.rows)
	}
	return append([]model.KnowledgeRow(nil), s.rows[:limit]...), nil
}

func TestPageAliasesShareKnowledgeStorage(t *testing.T) {
	base := memory.New()
	if _, err := base.EnsureBank(context.Background(), "pages", "", ""); err != nil {
		t.Fatal(err)
	}
	store := &pageTestStore{
		Store: base,
		rows: []model.KnowledgeRow{{
			ID:              "kp-alpha",
			BankID:          "pages",
			Kind:            "page",
			Name:            "Alpha",
			MentalModelID:   "mm-alpha",
			Description:     "What is Alpha?",
			Tags:            []string{"team"},
			Body:            "Alpha body",
			LastRefreshedAt: "2026-11-10T00:00:00Z",
		}},
	}
	srv := newEngineServerWithStore(t, store)
	defer srv.Close()

	for _, path := range []string{
		"/v1/default/banks/pages/pages/tree",
		"/v1/default/banks/pages/knowledge-base/tree",
	} {
		code, body := doJSON(t, http.MethodGet, srv.URL+path, nil)
		if code != http.StatusOK {
			t.Fatalf("%s returned %d: %v", path, code, body)
		}
		roots, ok := body["roots"].([]any)
		if !ok || len(roots) != 1 {
			t.Fatalf("%s roots = %#v", path, body["roots"])
		}
		root, _ := roots[0].(map[string]any)
		if root["id"] != "kp-alpha" {
			t.Fatalf("%s root = %#v", path, root)
		}
	}

	code, body := doJSON(t, http.MethodGet, srv.URL+"/v1/default/banks/pages/pages/kp-alpha", nil)
	if code != http.StatusOK {
		t.Fatalf("get page returned %d: %v", code, body)
	}
	if body["id"] != "kp-alpha" || body["name"] != "Alpha" {
		t.Fatalf("page = %#v", body)
	}
}

func TestPageResponseConversions(t *testing.T) {
	legacy := api.KnowledgeNode{
		ID:   "kp-one",
		Kind: api.KnowledgeNodeKindPage,
		Name: "One",
		Children: []api.KnowledgeNode{{
			ID:   "kf-child",
			Kind: api.KnowledgeNodeKindFolder,
			Name: "Child",
		}},
	}
	page := pageNodeFromKnowledge(legacy)
	if page.ID != "kp-one" || page.Kind != api.PageNodeKindPage || len(page.Children) != 1 {
		t.Fatalf("page node = %+v", page)
	}
	if page.Children[0].Kind != api.PageNodeKindFolder {
		t.Fatalf("child kind = %s", page.Children[0].Kind)
	}
}
