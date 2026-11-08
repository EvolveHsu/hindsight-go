package extract

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/llm"
)

const validFacts = `[{"text":"Alice lives in Berlin.","fact_type":"world","entities":["Alice"],"occurred_start":null,"occurred_end":null}]`

type providerFunc func(context.Context, llm.ChatRequest) (*llm.ChatResponse, error)

func (f providerFunc) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return f(ctx, req)
}

func response(content, finish string) *llm.ChatResponse {
	return &llm.ChatResponse{Choices: []llm.Choice{{
		Message: llm.Message{Content: content}, FinishReason: finish,
	}}}
}

func TestExtractRetryBounds(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options Options
		replies []*llm.ChatResponse
		budgets []int
		wantErr string
	}{
		{"default", Options{}, []*llm.ChatResponse{response(validFacts, "stop")}, []int{8192}, ""},
		{"length then success", Options{}, []*llm.ChatResponse{response(`[{"text":`, "length"), response(validFacts, "stop")}, []int{8192, 16384}, ""},
		{"valid but length", Options{}, []*llm.ChatResponse{response(validFacts, "length"), response(validFacts, "stop")}, []int{8192, 16384}, ""},
		{"missing reason truncated", Options{}, []*llm.ChatResponse{response(`[{"entities":["Alice"]`, ""), response(validFacts, "")}, []int{8192, 16384}, ""},
		{"ceiling", Options{MaxTokens: 8192, MaxTokensLimit: 10000}, []*llm.ChatResponse{response("", "length"), response("", "length")}, []int{8192, 10000}, "output truncated"},
		{"attempt ceiling", Options{MaxTokens: 100, MaxTokensLimit: 10000}, []*llm.ChatResponse{response("", "length"), response("", "length"), response("", "length")}, []int{100, 200, 400}, "output truncated"},
		{"no room", Options{MaxTokens: 100, MaxTokensLimit: 100}, []*llm.ChatResponse{response(validFacts, "length")}, []int{100}, "output truncated"},
		{"malformed stop", Options{}, []*llm.ChatResponse{response(`[{"text":`, "stop")}, []int{8192}, "decode extracted facts"},
		{"prose", Options{}, []*llm.ChatResponse{response("Here: "+validFacts, "")}, []int{8192}, "not a JSON array"},
		{"filtered", Options{}, []*llm.ChatResponse{response(validFacts, "content_filter")}, []int{8192}, "did not finish normally"},
		{"unknown reason", Options{}, []*llm.ChatResponse{response(validFacts, "SECRET_REASON")}, []int{8192}, "finish=other"},
		{"empty choices", Options{}, []*llm.ChatResponse{{}}, []int{8192}, "no choices"},
		{"nil response", Options{}, []*llm.ChatResponse{nil}, []int{8192}, "no choices"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var budgets []int
			p := providerFunc(func(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
				if len(budgets) >= len(tt.replies) {
					t.Fatal("unexpected model retry")
				}
				if req.Messages[1].Content != "Context: test\n\nContent:\noriginal" {
					t.Fatalf("retry changed input: %+v", req.Messages)
				}
				budgets = append(budgets, req.MaxTokens)
				return tt.replies[len(budgets)-1], nil
			})
			e, err := NewWithOptions(p, "model", tt.options)
			if err != nil {
				t.Fatal(err)
			}
			facts, err := e.Extract(context.Background(), "original", "test")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || facts != nil {
					t.Fatalf("facts=%v err=%v, want %q", facts, err, tt.wantErr)
				}
			} else if err != nil || len(facts) != 1 || facts[0].Text != "Alice lives in Berlin." {
				t.Fatalf("facts=%v err=%v", facts, err)
			}
			if !reflect.DeepEqual(budgets, tt.budgets) {
				t.Fatalf("budgets=%v want=%v", budgets, tt.budgets)
			}
		})
	}
}

func TestExtractProviderErrorAndCancellation(t *testing.T) {
	errProvider := errors.New("provider failed")
	calls := 0
	e := New(providerFunc(func(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
		calls++
		return nil, errProvider
	}), "")
	if _, err := e.Extract(context.Background(), "", ""); !errors.Is(err, errProvider) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Extract(ctx, "", ""); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	calls = 0
	e = New(providerFunc(func(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
		calls++
		cancel()
		return response("", "length"), nil
	}), "")
	if _, err := e.Extract(ctx, "", ""); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestParseFactsRequiresCompleteArray(t *testing.T) {
	for _, raw := range []string{validFacts, " \n" + validFacts + "\n ", "```json\n" + validFacts + "\n```", "```\n" + validFacts + "\n```", "```json\r\n" + validFacts + "\r\n```"} {
		if facts, err := parseFacts(raw); err != nil || len(facts) != 1 {
			t.Errorf("valid array: facts=%v err=%v", facts, err)
		}
	}
	for _, raw := range []string{
		"", "null", `{}`, "prefix " + validFacts, validFacts + " suffix",
		validFacts + validFacts, "```js\n" + validFacts + "\n```",
		"```json\n" + validFacts, `[{"text":"Alice","entities":["Alice"]`,
		`[{"text":false}]`, `[]`, `[{"text":"  "}]`,
	} {
		if facts, err := parseFacts(raw); err == nil || facts != nil {
			t.Errorf("accepted invalid response: %q -> %v", raw, facts)
		}
	}
	if _, err := parseFacts(`[{"entities":["Alice"]`); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost truncated JSON classification: %v", err)
	}
	facts, err := parseFacts(`[{"text":"  Alice  ","fact_type":"invalid"}]`)
	if err != nil || facts[0].Text != "Alice" || facts[0].FactType != "world" || facts[0].Entities == nil {
		t.Fatalf("normalization changed: %v %v", facts, err)
	}
}

func TestExtractionLogsDoNotContainPayloads(t *testing.T) {
	var output bytes.Buffer
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	}()
	e := New(providerFunc(func(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
		return response("SECRET_RESPONSE", "SECRET_FINISH"), nil
	}), "")
	_, _ = e.Extract(context.Background(), "SECRET_CONTENT", "SECRET_CONTEXT")
	e.Provider = providerFunc(func(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
		return nil, errors.New("SECRET_CREDENTIAL")
	})
	_, _ = e.Extract(context.Background(), "SECRET_CONTENT", "")
	if strings.Contains(output.String(), "SECRET") {
		t.Fatalf("payload leaked: %s", output.String())
	}
	if !strings.Contains(output.String(), "budget=8192") || !strings.Contains(output.String(), "result=provider_error") {
		t.Fatalf("missing safe diagnostics: %s", output.String())
	}
}
