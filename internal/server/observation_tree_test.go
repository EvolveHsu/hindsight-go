package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/memory"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

type observationTreeTestStore struct {
	*memory.Store
	units []model.Unit
}

func (s *observationTreeTestStore) Observations(context.Context, string, int) ([]model.Unit, error) {
	return append([]model.Unit(nil), s.units...), nil
}

func TestObservationTreeEndpoint(t *testing.T) {
	base := memory.New()
	if _, err := base.EnsureBank(context.Background(), "tree", "", ""); err != nil {
		t.Fatal(err)
	}
	store := &observationTreeTestStore{
		Store: base,
		units: []model.Unit{
			{
				ID: "obs-1", BankID: "tree", FactType: model.FactObservation,
				Text: "Alice likes green tea.", Tags: []string{"people"},
				SourceMemoryIds: []string{"fact-1"}, CreatedAt: "2026-11-20T10:00:00Z",
			},
			{
				ID: "obs-2", BankID: "tree", FactType: model.FactObservation,
				Text: "The service uses port 4567.", Tags: []string{"service"},
				SourceMemoryIds: []string{"fact-2"}, CreatedAt: "2026-11-20T11:00:00Z",
			},
		},
	}
	srv := newEngineServerWithStore(t, store)
	defer srv.Close()

	code, body := doJSON(t, http.MethodGet, srv.URL+"/v1/default/banks/tree/observations/tree?tags=people&tags_match=all", nil)
	if code != http.StatusOK {
		t.Fatalf("observation tree returned %d: %v", code, body)
	}
	if body["observation_count"] != float64(1) {
		t.Fatalf("observation count = %#v", body["observation_count"])
	}
	files, _ := body["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files = %#v", body["files"])
	}
	file, _ := files[0].(map[string]any)
	if file["path"] != "people.md" || file["kind"] != "topic" {
		t.Fatalf("file = %#v", file)
	}
}
