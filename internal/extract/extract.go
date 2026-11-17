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
	"unicode"

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
Return ONLY one valid JSON object. The object must have a single top-level
"facts" key whose value is an array. Do not wrap it in markdown.
Each item must have:
  "text": a self-contained fact, preserving names, dates, quantities and concrete details,
  "fact_type": "world" or "experience",
  "entities": an array of entity names mentioned in the fact,
  "occurred_start": ISO-8601 date/time or null,
  "occurred_end": ISO-8601 date/time or null.
Split compound sentences into separate facts. Do not invent facts. Omit transient filler.`

const userInstructions = `Extract durable atomic memories from the content below.
Return ONLY one valid JSON object, without markdown, prose or explanation.
Shape:
{"facts":[{"text":"...","fact_type":"world","entities":[],"occurred_start":null,"occurred_end":null}]}
Use "experience" for events the speaker experienced; otherwise use "world".
Do not invent facts.`

// Extract returns the facts in the order the model produced them.
func (e *Extractor) Extract(ctx context.Context, content, contextLabel string) ([]Fact, error) {
	if e == nil || e.Provider == nil {
		return nil, fmt.Errorf("extractor provider is not configured")
	}
	options, err := e.options.normalized()
	if err != nil {
		return nil, err
	}
	chunks, err := contentChunks(content, contextLabel, options)
	if err != nil {
		return nil, err
	}
	var out []Fact
	for index, chunk := range chunks {
		facts, err := e.extractChunk(ctx, chunk, contextLabel, options, index+1, len(chunks))
		if err != nil {
			return nil, err
		}
		out = append(out, facts...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("extractor returned no facts")
	}
	return out, nil
}

func (e *Extractor) extractChunk(ctx context.Context, content, contextLabel string, options Options, chunkIndex, chunkCount int) ([]Fact, error) {
	user := userInstructions + "\n\nContext: " + contextLabel + "\n\nContent:\n" + content
	budget := options.MaxTokens
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := e.Provider.Chat(ctx, llm.ChatRequest{
			Model:          e.Model,
			Messages:       []llm.Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: user}},
			Temperature:    0,
			MaxTokens:      budget,
			ResponseFormat: map[string]any{"type": "json_object"},
		})
		if err != nil {
			log.Printf("retain extraction chunk=%d/%d attempt=%d budget=%d result=provider_error", chunkIndex, chunkCount, attempt, budget)
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if resp == nil || len(resp.Choices) == 0 {
			log.Printf("retain extraction chunk=%d/%d attempt=%d budget=%d result=no_choices", chunkIndex, chunkCount, attempt, budget)
			return nil, fmt.Errorf("extractor returned no choices")
		}
		choice := resp.Choices[0]
		finish := finishCategory(choice.FinishReason)
		log.Printf("retain extraction chunk=%d/%d attempt=%d budget=%d finish=%s prompt_tokens=%d completion_tokens=%d reasoning_tokens=%d",
			chunkIndex, chunkCount, attempt, budget, finish, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.ReasoningTokens())
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
				log.Printf("retain extraction chunk=%d/%d attempt=%d result=invalid_json shape=%s",
					chunkIndex, chunkCount, attempt, responseShape(choice.Message.Content))
				return nil, parseErr
			}
			log.Printf("retain extraction chunk=%d/%d attempt=%d result=success facts=%d",
				chunkIndex, chunkCount, attempt, len(facts))
			return facts, nil
		}
		log.Printf("retain extraction chunk=%d/%d attempt=%d result=truncated", chunkIndex, chunkCount, attempt)
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
	if raw == "" {
		return nil, fmt.Errorf("extractor response is empty")
	}
	if fenced, ok := fencedJSON(raw); ok {
		raw = fenced
	}
	if facts, err := decodeFactsJSON(raw); err == nil {
		return normalizeFacts(facts)
	} else if errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("decode extracted facts: %w", err)
	}

	var decoded [][]Fact
	var truncated error
	for _, candidate := range jsonCandidates(raw) {
		facts, err := decodeFactsJSON(candidate)
		if err == nil {
			decoded = append(decoded, facts)
			continue
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			truncated = err
		}
	}
	switch len(decoded) {
	case 1:
		return normalizeFacts(decoded[0])
	case 0:
		if truncated != nil {
			return nil, fmt.Errorf("decode extracted facts: %w", truncated)
		}
		return nil, fmt.Errorf("extractor response is not a JSON array (shape=%s, length=%d)", responseShape(raw), len(raw))
	default:
		return nil, fmt.Errorf("extractor response contains multiple JSON values")
	}
}

func decodeFactsJSON(raw string) ([]Fact, error) {
	var facts []Fact
	if err := decodeJSONValue(raw, &facts); err == nil {
		return facts, nil
	}
	var envelope map[string]json.RawMessage
	if err := decodeJSONValue(raw, &envelope); err != nil {
		return nil, err
	}
	for _, key := range []string{"facts", "items", "memories", "results", "data"} {
		if value, ok := envelope[key]; ok {
			if err := json.Unmarshal(value, &facts); err == nil {
				return facts, nil
			}
		}
	}
	if _, ok := envelope["text"]; ok {
		var fact Fact
		if err := json.Unmarshal([]byte(raw), &fact); err == nil {
			return []Fact{fact}, nil
		}
	}
	return nil, fmt.Errorf("JSON object does not contain a facts array")
}

func decodeJSONValue(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func normalizeFacts(facts []Fact) ([]Fact, error) {
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

func fencedJSON(raw string) (string, bool) {
	start := strings.Index(raw, "```")
	if start < 0 {
		return "", false
	}
	rest := raw[start+3:]
	newline := strings.IndexByte(rest, '\n')
	if newline < 0 {
		return "", false
	}
	header := strings.TrimSpace(rest[:newline])
	if header != "" && !strings.EqualFold(header, "json") {
		return "", false
	}
	body := rest[newline+1:]
	end := strings.LastIndex(body, "```")
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(body[:end]), true
}

// jsonCandidates returns balanced top-level JSON values embedded in prose.
func jsonCandidates(raw string) []string {
	var out []string
	for i := 0; i < len(raw); i++ {
		if raw[i] != '[' && raw[i] != '{' {
			continue
		}
		end, ok := balancedJSONEnd(raw, i)
		if !ok {
			continue
		}
		out = append(out, raw[i:end])
		i = end - 1
	}
	return out
}

func balancedJSONEnd(raw string, start int) (int, bool) {
	open := raw[start]
	close := byte(']')
	if open == '{' {
		close = '}'
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(raw); i++ {
		ch := raw[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '[':
			if open == '[' {
				depth++
			}
		case '{':
			if open == '{' {
				depth++
			}
		case ']':
			if open == '[' {
				depth--
			}
		case '}':
			if open == '{' {
				depth--
			}
		}
		if depth == 0 && i > start && ch == close {
			return i + 1, true
		}
	}
	return 0, false
}

func responseShape(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "empty"
	}
	switch trimmed[0] {
	case '[':
		return "array"
	case '{':
		return "object"
	case '`':
		return "fenced"
	default:
		return "text"
	}
}

func contentChunks(content, contextLabel string, options Options) ([]string, error) {
	fixed := estimateTokens(systemPrompt) + estimateTokens(userInstructions) +
		estimateTokens("Context: "+contextLabel+"\n\nContent:\n") + options.MaxTokens + 64
	if fixed >= options.ContextTokens {
		return nil, fmt.Errorf("LLM context budget %d is too small for retain instructions", options.ContextTokens)
	}
	limit := options.ContextTokens - fixed
	if estimateTokens(content) <= limit {
		return []string{content}, nil
	}
	return splitTokenBudget(content, limit), nil
}

func splitTokenBudget(content string, limit int) []string {
	if limit <= 0 {
		limit = 1
	}
	var out []string
	var b strings.Builder
	used, asciiRun := 0, 0
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
		used, asciiRun = 0, 0
	}
	for _, r := range content {
		cost := 0
		switch {
		case unicode.IsSpace(r):
			asciiRun = 0
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			asciiRun = 0
			cost = 1
		default:
			asciiRun++
			if asciiRun%3 == 0 {
				cost = 1
			}
		}
		if used+cost > limit {
			flush()
			if unicode.IsSpace(r) {
				continue
			}
			cost = 1
		}
		b.WriteRune(r)
		used += cost
	}
	flush()
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func estimateTokens(s string) int {
	tokens, asciiRun := 0, 0
	flush := func() {
		if asciiRun > 0 {
			tokens += (asciiRun + 2) / 3
			asciiRun = 0
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			flush()
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			flush()
			tokens++
		default:
			asciiRun++
		}
	}
	flush()
	return tokens
}
