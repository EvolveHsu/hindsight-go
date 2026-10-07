// Package embeddings defines the minimal OpenAI-compatible embeddings client
// used by the PostgreSQL recall path. It is intentionally small: retain sends
// document strings, recall sends one query string, and both sides validate the
// vector shape before the values reach pgvector.
package embeddings

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// InputType selects the optional asymmetric prefix for the request.
type InputType string

const (
	InputDocument InputType = "document"
	InputQuery    InputType = "query"
)

// Embedder is the storage-layer seam. Tests inject fakes; main injects Client.
type Embedder interface {
	Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error)
}

// Client talks to an OpenAI-compatible /v1/embeddings endpoint. BaseURL should
// include the version segment, for example https://api.example.com/v1.
type Client struct {
	BaseURL        string
	APIKey         string
	Model          string
	Dimensions     int
	BatchSize      int
	Retries        int
	QueryPrefix    string
	DocumentPrefix string
	HTTP           *http.Client
}

// NewClient builds a client with the same timeout policy as the chat client.
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		Model:     model,
		BatchSize: 64,
		Retries:   2,
		HTTP:      &http.Client{Timeout: 120 * time.Second},
	}
}

type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type embedDatum struct {
	Index     *int            `json:"index"`
	Embedding json.RawMessage `json:"embedding"`
}

type embedResponse struct {
	Data  []embedDatum `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Embed encodes every input and returns one vector per text in input order.
func (c *Client) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	if c.BaseURL == "" || c.Model == "" {
		return nil, fmt.Errorf("embeddings: base URL and model are required")
	}
	batchSize := c.BatchSize
	if batchSize <= 0 {
		batchSize = 64
	}

	prepared := make([]string, len(texts))
	for i, text := range texts {
		switch inputType {
		case InputQuery:
			prepared[i] = c.QueryPrefix + text
		default:
			prepared[i] = c.DocumentPrefix + text
		}
	}

	out := make([][]float32, 0, len(prepared))
	for start := 0; start < len(prepared); start += batchSize {
		end := start + batchSize
		if end > len(prepared) {
			end = len(prepared)
		}
		vectors, err := c.embedBatch(ctx, prepared[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	if len(out) != len(texts) {
		return nil, fmt.Errorf("embeddings: returned %d vectors for %d inputs", len(out), len(texts))
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := embedRequest{
		Model:          c.Model,
		Input:          texts,
		Dimensions:     c.Dimensions,
		EncodingFormat: "float",
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	buildRequest := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/embeddings", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		return req, nil
	}

	var body []byte
	var status int
	for attempt := 0; ; attempt++ {
		req, err := buildRequest()
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			if attempt < c.Retries {
				if sleepErr := sleepContext(ctx, retryDelay(attempt)); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return nil, err
		}
		body, err = io.ReadAll(resp.Body)
		status = resp.StatusCode
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if retryableStatus(status) && attempt < c.Retries {
			if sleepErr := sleepContext(ctx, retryDelay(attempt)); sleepErr != nil {
				return nil, sleepErr
			}
			continue
		}
		break
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("embeddings %d: %s", status, truncate(string(body), 400))
	}

	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("embeddings: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: provider returned %d data rows for %d inputs", len(parsed.Data), len(texts))
	}

	vectors := make([][]float32, len(texts))
	seen := make([]bool, len(texts))
	hasIndex := false
	for _, datum := range parsed.Data {
		if datum.Index != nil {
			hasIndex = true
			break
		}
	}
	if !hasIndex {
		for i, datum := range parsed.Data {
			vector, err := decodeVector(datum.Embedding)
			if err != nil {
				return nil, fmt.Errorf("embeddings: data[%d]: %w", i, err)
			}
			if err := c.validateVector(vector, i); err != nil {
				return nil, err
			}
			vectors[i] = vector
		}
		return vectors, nil
	}

	for i, datum := range parsed.Data {
		if datum.Index == nil {
			return nil, fmt.Errorf("embeddings: data[%d] is missing index", i)
		}
		idx := *datum.Index
		if idx < 0 || idx >= len(texts) {
			return nil, fmt.Errorf("embeddings: data[%d] index %d out of range", i, idx)
		}
		if seen[idx] {
			return nil, fmt.Errorf("embeddings: duplicate index %d", idx)
		}
		vector, err := decodeVector(datum.Embedding)
		if err != nil {
			return nil, fmt.Errorf("embeddings: data[%d]: %w", i, err)
		}
		if err := c.validateVector(vector, idx); err != nil {
			return nil, err
		}
		vectors[idx] = vector
		seen[idx] = true
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("embeddings: response missing index %d", i)
		}
	}
	return vectors, nil
}

func (c *Client) validateVector(vector []float32, index int) error {
	if len(vector) == 0 {
		return fmt.Errorf("embeddings: vector %d is empty", index)
	}
	if c.Dimensions > 0 && len(vector) != c.Dimensions {
		return fmt.Errorf("embeddings: vector %d has dimension %d, want %d", index, len(vector), c.Dimensions)
	}
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("embeddings: vector %d contains a non-finite value", index)
		}
	}
	return nil
}

// decodeVector accepts the float-array shape used by OpenAI and TEI, and the
// base64 shape some OpenAI-compatible gateways emit when encoding_format is
// ignored.
func decodeVector(raw json.RawMessage) ([]float32, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("empty embedding")
	}
	if trimmed[0] == '[' {
		var values []float64
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return nil, err
		}
		out := make([]float32, len(values))
		for i, value := range values {
			out[i] = float32(value)
		}
		return out, nil
	}
	if trimmed[0] == '"' {
		var encoded string
		if err := json.Unmarshal(trimmed, &encoded); err != nil {
			return nil, err
		}
		blob, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode base64 embedding: %w", err)
		}
		if len(blob)%4 != 0 {
			return nil, fmt.Errorf("base64 embedding length %d is not a multiple of 4", len(blob))
		}
		out := make([]float32, len(blob)/4)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported embedding shape")
}

// Dimension returns the configured dimension, or zero when the caller did not
// pin one. It is used by startup diagnostics.
func (c *Client) Dimension() int { return c.Dimensions }

// Cosine computes cosine similarity for two vectors. It is used by the
// in-process and text-column fallbacks; pgvector computes the same value in
// SQL for the production path.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		av := float64(a[i])
		bv := float64(b[i])
		dot += av * bv
		normA += av * av
		normB += bv * bv
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryDelay(attempt int) time.Duration {
	delay := 200 * time.Millisecond * time.Duration(1<<attempt)
	if delay > 2*time.Second {
		return 2 * time.Second
	}
	return delay
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
