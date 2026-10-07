// Package consolidate implements the observation synthesis loop: take
// unconsolidated raw facts, ask the model to merge them into observations
// (creates / updates / deletes), and persist the result.
//
// The lite shape mirrors the upstream batch contract exactly: the model returns
// {"creates":[{text, source_fact_ids}], "updates":[{text, observation_id,
// source_fact_ids}], "deletes":[{observation_id}]}, and the merge-vs-create
// decision rules come from the same prompt sections (upstream
// engine/consolidation/prompts.py). Lite omits the semantic-dedup LLM
// adjudication pass and the scope fan-out; those are the documented next tier.
package consolidate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Source is the storage slice the consolidator needs.
type Source interface {
	// UnconsolidatedFacts returns raw (world/experience) facts with
	// consolidated_at IS NULL, oldest first.
	UnconsolidatedFacts(ctx context.Context, bankID string, limit int) ([]model.Unit, error)
	// Observations returns existing observation units for dedup context.
	Observations(ctx context.Context, bankID string, limit int) ([]model.Unit, error)
	// ApplyBatch persists one batch result atomically: creates insert new
	// observation units (consolidated_at=now, source ids attached), updates
	// rewrite text + merge sources + bump consolidated_at, deletes remove.
	// Fact-level consolidated_at is stamped for every source id touched.
	ApplyBatch(ctx context.Context, bankID string, b Batch) error
}

// CreateAction is one new observation.
type CreateAction struct {
	Text          string   `json:"text"`
	SourceFactIDs []string `json:"source_fact_ids"`
	Reason        string   `json:"reason,omitempty"`
}

// UpdateAction merges facts into an existing observation.
type UpdateAction struct {
	Text          string   `json:"text"`
	ObservationID string   `json:"observation_id"`
	SourceFactIDs []string `json:"source_fact_ids"`
	Reason        string   `json:"reason,omitempty"`
}

// DeleteAction removes an observation (restated / meaningless only).
type DeleteAction struct {
	ObservationID string `json:"observation_id"`
	Reason        string `json:"reason,omitempty"`
}

// Batch is the model's output for one batch.
type Batch struct {
	Creates []CreateAction `json:"creates"`
	Updates []UpdateAction `json:"updates"`
	Deletes []DeleteAction `json:"deletes"`
}

// ErrMalformed is returned when the model's JSON does not match the contract.
var ErrMalformed = errors.New("consolidation response malformed")

// Config tunes the loop.
type Config struct {
	Model       string
	Temperature float64
	MaxTokens   int
	BatchSize   int // facts per LLM call
	MaxBatches  int // hard cap per Run
}

// DefaultConfig returns the lite defaults (upstream: batch 50 / llm batch 8;
// lite starts conservative).
func DefaultConfig() Config {
	return Config{
		Model:       "",
		Temperature: 0.1,
		MaxTokens:   2048,
		BatchSize:   20,
		MaxBatches:  10,
	}
}

// Consolidator runs batches.
type Consolidator struct {
	Provider Provider
	Cfg      Config
}

// Provider is the model interface (llm.Client satisfies it; tests fake it).
type Provider interface {
	Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

// Stats reports what one Run did.
type Stats struct {
	Batches  int
	Creates  int
	Updates  int
	Deletes  int
	DocsSeen int
	UsageIn  int
	UsageOut int
}

// Run processes up to MaxBatches of unconsolidated facts.
func (c *Consolidator) Run(ctx context.Context, src Source, bankID string) (*Stats, error) {
	cfg := c.Cfg
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 20
	}
	if cfg.MaxBatches <= 0 {
		cfg.MaxBatches = 10
	}

	stats := &Stats{}
	for i := 0; i < cfg.MaxBatches; i++ {
		facts, err := src.UnconsolidatedFacts(ctx, bankID, cfg.BatchSize)
		if err != nil {
			return nil, err
		}
		if len(facts) == 0 {
			break
		}
		stats.DocsSeen += len(facts)

		obs, err := src.Observations(ctx, bankID, 50)
		if err != nil {
			return nil, err
		}

		batch, usage, err := c.runBatch(ctx, facts, obs)
		if err != nil {
			return nil, err
		}
		if err := src.ApplyBatch(ctx, bankID, *batch); err != nil {
			return nil, err
		}
		stats.UsageIn += usage.PromptTokens
		stats.UsageOut += usage.CompletionTokens
		stats.Batches++
		stats.Creates += len(batch.Creates)
		stats.Updates += len(batch.Updates)
		stats.Deletes += len(batch.Deletes)

		// break early when the source is drained (batch smaller than requested)
		if len(facts) < cfg.BatchSize {
			break
		}
	}
	return stats, nil
}

// runBatch does one LLM call for one batch of facts.
func (c *Consolidator) runBatch(ctx context.Context, facts, obs []model.Unit) (*Batch, llm.Usage, error) {
	var factsB strings.Builder
	for _, f := range facts {
		fmt.Fprintf(&factsB, "[%s] %s\n", f.ID, f.Text)
	}
	var obsB strings.Builder
	if len(obs) == 0 {
		obsB.WriteString("(none yet)\n")
	}
	for _, o := range obs {
		fmt.Fprintf(&obsB, `{"id": %q, "text": %q, "proof_count": 1}`+"\n", o.ID, o.Text)
	}

	user := fmt.Sprintf("## INPUT\n\n### New facts\n\n%s\n### Existing observations\n\n%s", factsB.String(), obsB.String())

	resp, err := c.Provider.Chat(ctx, llm.ChatRequest{
		Model:       c.Cfg.Model,
		Temperature: c.Cfg.Temperature,
		MaxTokens:   c.Cfg.MaxTokens,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt()},
			{Role: llm.RoleUser, Content: user},
		},
	})
	if err != nil {
		return nil, llm.Usage{}, err
	}
	if len(resp.Choices) == 0 {
		return nil, llm.Usage{}, ErrMalformed
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	content = stripFence(content)

	var batch Batch
	if err := json.Unmarshal([]byte(content), &batch); err != nil {
		return nil, llm.Usage{}, fmt.Errorf("%w: %v (raw: %s)", ErrMalformed, err, truncate(content, 300))
	}
	return &batch, resp.Usage, nil
}

// stripFence removes a markdown code fence the model may add.
func stripFence(s string) string {
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// systemPrompt carries the upstream PROCESSING RULES + DECISION GUIDE +
// OUTPUT FORMAT sections lite keeps. The rule text is quoted from upstream
// engine/consolidation/prompts.py so behavior matches.
func systemPrompt() string {
	var b strings.Builder
	b.WriteString("You are a memory consolidation agent. You merge newly stored facts into durable observations.\n\n")
	b.WriteString("## MISSION\nTrack anything notable in the new facts: names, numbers, dates, places, events, decisions, claims, relationships, and recurring patterns.\n\n")
	b.WriteString("## PROCESSING RULES\n\n")
	b.WriteString("1. PREFER UPDATE OVER CREATE (when there is something to merge with): if new facts describe the same canonical event, statement, decision, claim, or recurring pattern already covered by an existing observation, UPDATE that observation and attach the new facts as evidence. Do NOT create a near-duplicate sibling. When the EXISTING OBSERVATIONS list is empty, or no existing observation covers the same facet as a new fact, CREATE a new observation.\n")
	b.WriteString("2. ONE OBSERVATION PER DISTINCT FACET: each observation tracks exactly one specific facet - a count, a named entity, a relationship, a decision, an event. Never merge different facets into one observation.\n")
	b.WriteString("3. MATCH BY ENTITY/FACET, NOT TOPIC: \"Sold item X\" updates only the X observation. Do not update observations about different entities just because they share a general topic.\n")
	b.WriteString("4. STATE CHANGES - UPDATE CONCISELY: when a fact changes the state of something, UPDATE the matching observation to reflect the current state. Keep it concise and focused on that facet.\n")
	b.WriteString("5. PRESERVE HISTORY: observations that record significant events are important history - never DELETE them. Only delete an observation when it is restated identically or truly meaningless.\n")
	b.WriteString("6. NO COMPUTATION: never calculate, derive, or adjust numeric values. Only update a count when the user explicitly states a new count.\n")
	b.WriteString("7. KEEP DISTINCT TOPICS DISTINCT: do not merge observations about different people, entities, or unrelated topics.\n\n")
	b.WriteString("## DECISION GUIDE\n\n")
	b.WriteString("- Same canonical event, decision, claim, or facet as an existing observation -> UPDATE (use observation_id + source_fact_ids).\n")
	b.WriteString("- New durable knowledge with no existing match -> CREATE (use source_fact_ids).\n")
	b.WriteString("- Purely ephemeral facts -> omit them.\n\n")
	b.WriteString("## OUTPUT FORMAT\n\nReturn ONLY a JSON object with three arrays: creates, updates, deletes.\n")
	b.WriteString("- creates: [{\"text\": \"...\", \"source_fact_ids\": [\"<fact id from the input>\"]}]\n")
	b.WriteString("- updates: [{\"text\": \"...\", \"observation_id\": \"<existing id>\", \"source_fact_ids\": [\"<fact id>\"]}]\n")
	b.WriteString("- deletes: [{\"observation_id\": \"<existing id>\"}]\n")
	b.WriteString("Every entry may include a \"reason\". Use exactly the ids given in the input. Output raw JSON, no markdown fences.\n")
	return b.String()
}
