// Package extract turns raw retained content into atomic facts using the
// configured OpenAI-compatible chat model.
package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	options  Options
}

// New builds an extractor. Model may be empty when the provider ignores it.
func New(provider Provider, model string) *Extractor {
	return &Extractor{Provider: provider, Model: model}
}

// NewWithOptions builds an extractor with validated output budgets.
func NewWithOptions(provider Provider, model string, options Options) (*Extractor, error) {
	options, err := options.normalized()
	if err != nil {
		return nil, err
	}
	return &Extractor{Provider: provider, Model: model, options: options}, nil
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
	options, err := e.options.normalized()
	if err != nil {
		return nil, err
	}
	user := "Context: " + contextLabel + "\n\nContent:\n" + content
	budget := options.MaxTokens
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := e.Provider.Chat(ctx, llm.ChatRequest{
			Model:       e.Model,
			Messages:    []llm.Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: user}},
			Temperature: 0,
			MaxTokens:   budget,
		})
		if err != nil {
			log.Printf("retain extraction attempt=%d budget=%d result=provider_error", attempt, budget)
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if resp == nil || len(resp.Choices) == 0 {
			log.Printf("retain extraction attempt=%d budget=%d result=no_choices", attempt, budget)
			return nil, fmt.Errorf("extractor returned no choices")
		}
		choice := resp.Choices[0]
		finish := finishCategory(choice.FinishReason)
		log.Printf("retain extraction attempt=%d budget=%d finish=%s prompt_tokens=%d completion_tokens=%d reasoning_tokens=%d",
			attempt, budget, finish, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.ReasoningTokens())
		var facts []Fact
		var parseErr error
		if finish == "stop" || finish == "missing" {
			facts, parseErr = parseFacts(choice.Message.Content)
		} else if finish != "length" {
			return nil, fmt.Errorf("extractor did not finish normally (finish=%s)", finish)
		}
		truncated := finish == "length" || (finish == "missing" && errors.Is(parseErr, io.ErrUnexpectedEOF))
		if !truncated {
			if parseErr != nil {
				log.Printf("retain extraction attempt=%d result=invalid_json", attempt)
				return nil, parseErr
			}
			log.Printf("retain extraction attempt=%d result=success facts=%d", attempt, len(facts))
			return facts, nil
		}
		log.Printf("retain extraction attempt=%d result=truncated", attempt)
		if budget >= options.MaxTokensLimit || attempt == maxAttempts {
			return nil, fmt.Errorf("extractor output truncated after %d attempt(s), budget=%d, limit=%d; no facts stored",
				attempt, budget, options.MaxTokensLimit)
		}
		budget = min(budget*2, options.MaxTokensLimit)
	}
	return nil, fmt.Errorf("extractor exhausted attempts")
}

func finishCategory(reason string) string {
	switch reason {
	case "":
		return "missing"
	case "stop", "length", "tool_calls", "content_filter", "function_call":
		return reason
	default:
		return "other"
	}
}

func parseFacts(raw string) ([]Fact, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		header, body, ok := strings.Cut(raw, "\n")
		header = strings.TrimSpace(header)
		if !ok || (header != "```" && !strings.EqualFold(header, "```json")) || !strings.HasSuffix(body, "```") {
			return nil, fmt.Errorf("extractor response has an invalid JSON fence")
		}
		raw = strings.TrimSpace(strings.TrimSuffix(body, "```"))
	}
	if !strings.HasPrefix(raw, "[") {
		return nil, fmt.Errorf("extractor response is not a JSON array")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var facts []Fact
	if err := decoder.Decode(&facts); err != nil {
		return nil, fmt.Errorf("decode extracted facts: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("extractor response contains trailing data")
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
