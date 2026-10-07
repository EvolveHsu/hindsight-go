// Package extract turns raw retained content into atomic facts using the
// configured OpenAI-compatible chat model.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/EvolveHsu/hindsight-go/internal/llm"
)

// Provider is the chat seam shared with reflect.
type Provider interface {
	Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

// Fact is one extracted atomic memory.
type Fact struct {
	Text          string   `json:"text"`
	FactType      string   `json:"fact_type"`
	Entities      []string `json:"entities"`
	OccurredStart string   `json:"occurred_start"`
	OccurredEnd   string   `json:"occurred_end"`
}

// Extractor renders the retain prompt and parses the JSON response.
type Extractor struct {
	Provider Provider
	Model    string
}

// New builds an extractor. Model may be empty when the provider ignores it.
func New(provider Provider, model string) *Extractor {
	return &Extractor{Provider: provider, Model: model}
}

const systemPrompt = `You extract durable atomic memories from raw content.
Return ONLY a JSON array. Do not wrap it in markdown.
Each item must have:
  "text": a self-contained fact, preserving names, dates, quantities and concrete details,
  "fact_type": "world" or "experience",
  "entities": an array of entity names mentioned in the fact,
  "occurred_start": ISO-8601 date/time or null,
  "occurred_end": ISO-8601 date/time or null.
Split compound sentences into separate facts. Do not invent facts. Omit transient filler.`

// Extract returns the facts in the order the model produced them.
func (e *Extractor) Extract(ctx context.Context, content, contextLabel string) ([]Fact, error) {
	if e == nil || e.Provider == nil {
		return nil, fmt.Errorf("extractor provider is not configured")
	}
	user := "Context: " + contextLabel + "\n\nContent:\n" + content
	resp, err := e.Provider.Chat(ctx, llm.ChatRequest{
		Model:       e.Model,
		Messages:    []llm.Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: user}},
		Temperature: 0,
		MaxTokens:   2048,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || len(resp.Choices) == 0 {
		return nil, fmt.Errorf("extractor returned no choices")
	}
	return parseFacts(resp.Choices[0].Message.Content)
}

func parseFacts(raw string) ([]Fact, error) {
	start := strings.IndexByte(raw, '[')
	end := strings.LastIndexByte(raw, ']')
	if start < 0 || end < start {
		return nil, fmt.Errorf("extractor response is not a JSON array")
	}
	var facts []Fact
	if err := json.Unmarshal([]byte(raw[start:end+1]), &facts); err != nil {
		return nil, fmt.Errorf("decode extracted facts: %w", err)
	}
	out := make([]Fact, 0, len(facts))
	for _, fact := range facts {
		fact.Text = strings.TrimSpace(fact.Text)
		if fact.Text == "" {
			continue
		}
		if fact.FactType != "experience" {
			fact.FactType = "world"
		}
		if fact.Entities == nil {
			fact.Entities = []string{}
		}
		out = append(out, fact)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("extractor returned no facts")
	}
	return out, nil
}
