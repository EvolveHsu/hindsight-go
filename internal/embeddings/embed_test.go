package embeddings

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestEmbedRequestShapeAndOrder(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"index": 1, "embedding": [0, 1, 0]},
				{"index": 0, "embedding": [1, 0, 0]}
			]
		}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL+"/v1", "secret", "test-model")
	client.Dimensions = 3
	vectors, err := client.Embed(context.Background(), []string{"first", "second"}, InputDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotBody.Model != "test-model" || gotBody.Dimensions != 3 || gotBody.EncodingFormat != "float" {
		t.Errorf("body = %+v", gotBody)
	}
	if len(gotBody.Input) != 2 || gotBody.Input[0] != "first" || gotBody.Input[1] != "second" {
		t.Fatalf("input = %v", gotBody.Input)
	}
	if len(vectors) != 2 || vectors[0][0] != 1 || vectors[1][1] != 1 {
		t.Fatalf("vectors = %v", vectors)
	}
}

func TestEmbedBatchesInputs(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body embedRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		calls++
		mu.Unlock()
		data := make([]map[string]any, len(body.Input))
		for i := range body.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{float64(i), 1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	client := NewClient(srv.URL+"/v1", "", "m")
	client.BatchSize = 2
	vectors, err := client.Embed(context.Background(), []string{"a", "b", "c"}, InputQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if len(vectors) != 3 || vectors[2][0] != 0 {
		t.Fatalf("vectors = %v", vectors)
	}
}

func TestEmbedAcceptsBase64Vectors(t *testing.T) {
	blob := make([]byte, 8)
	binary.LittleEndian.PutUint32(blob[0:4], math.Float32bits(1.5))
	binary.LittleEndian.PutUint32(blob[4:8], math.Float32bits(-2.5))
	encoded := base64.StdEncoding.EncodeToString(blob)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": encoded}},
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL+"/v1", "", "m")
	vectors, err := client.Embed(context.Background(), []string{"x"}, InputDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 1 || vectors[0][0] != 1.5 || vectors[0][1] != -2.5 {
		t.Fatalf("vectors = %v", vectors)
	}
}

func TestEmbedRetriesTransientFailures(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"warming up"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]}]}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL+"/v1", "", "m")
	client.Retries = 2
	vectors, err := client.Embed(context.Background(), []string{"x"}, InputDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if calls != 2 || len(vectors) != 1 || vectors[0][1] != 2 {
		t.Fatalf("calls = %d, vectors = %v", calls, vectors)
	}
}

func TestEmbedDimensionMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]}]}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL+"/v1", "", "m")
	client.Dimensions = 3
	if _, err := client.Embed(context.Background(), []string{"x"}, InputDocument); err == nil {
		t.Fatal("Embed succeeded with a dimension mismatch")
	}
}

func TestCosine(t *testing.T) {
	if got := Cosine([]float32{1, 0}, []float32{1, 0}); got != 1 {
		t.Errorf("identical cosine = %v", got)
	}
	if got := Cosine([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Errorf("orthogonal cosine = %v", got)
	}
	if got := Cosine([]float32{1, 0}, []float32{-1, 0}); got != -1 {
		t.Errorf("opposite cosine = %v", got)
	}
}
