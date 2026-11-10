package server

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/memory"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

func TestUpdateDocumentIncrementalTags(t *testing.T) {
	store := memory.New()
	if _, err := store.EnsureBank(context.Background(), "tags", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Retain(context.Background(), "tags", []model.RetainItem{{
		Content:    "A tagged fact",
		DocumentID: "doc",
		Tags:       []string{"keep", "remove"},
		FactType:   model.FactExperience,
	}}); err != nil {
		t.Fatal(err)
	}

	srv := newEngineServerWithStore(t, store)
	defer srv.Close()

	code, body := doJSON(t, http.MethodPatch, srv.URL+"/v1/default/banks/tags/documents/doc", map[string]any{
		"remove_tags": []string{"remove"},
		"add_tags":    []string{"new"},
	})
	if code != http.StatusOK {
		t.Fatalf("patch tags returned %d: %v", code, body)
	}
	doc, err := store.GetDocument(context.Background(), "tags", "doc")
	if err != nil {
		t.Fatal(err)
	}
	if doc == nil || !reflect.DeepEqual(doc.Tags, []string{"keep", "new"}) {
		t.Fatalf("document tags = %+v", doc)
	}

	code, body = doJSON(t, http.MethodPatch, srv.URL+"/v1/default/banks/tags/documents/doc", map[string]any{
		"tags":     []string{"only"},
		"add_tags": []string{"mixed"},
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("mixed tag modes returned %d: %v", code, body)
	}
}
