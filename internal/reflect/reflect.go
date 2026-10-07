// Package reflect implements the reflect agent: a tool-calling loop over the
// bank's memories that answers a question and reports the evidence it used.
//
// The lite loop mirrors the upstream shape (hierarchical retrieval: mental
// models, observations, recall) but collapses to the tools lite supports:
// recall (world+experience), search_observations (observation type), and done.
// The provider is the OpenAI-compatible client in internal/llm, injectable for
// tests (see Provider).
package reflect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/EvolveHsu/hindsight-go/internal/llm"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Provider abstracts the chat model so tests can drive the loop
// deterministically.
type Provider interface {
	Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

// Config tunes one reflect run.
type Config struct {
	Model       string
	Temperature float64
	MaxTokens   int
	MaxTurns    int // hard cap on tool-call iterations
	Budget      int // recall result budget per query
}

// DefaultConfig returns the lite reflect defaults.
func DefaultConfig() Config {
	return Config{
		Model:       "",
		Temperature: 0.2,
		MaxTokens:   1024,
		MaxTurns:    6,
		Budget:      20,
	}
}

// Tool names.
const (
	ToolRecall             = "recall"
	ToolSearchObservations = "search_observations"
	ToolDone               = "done"
)

// Evidence accumulates what the loop used.
type Evidence struct {
	Memories     []model.Unit
	MentalModels []model.Unit // observations typed as such, lite keeps one list
	Directives   []Directive
}

// Directive is a bank rule the answer must follow.
type Directive struct {
	ID      string
	Name    string
	Content string
}

// MemorySource is the read-only slice of storage the agent may use.
type MemorySource interface {
	Recall(ctx context.Context, bankID string, opt model.RecallOptions) ([]model.RecallHit, error)
	ListDirectives(ctx context.Context, bankID string) ([]Directive, error)
	BankMission(ctx context.Context, bankID string) (string, error)
}

// Result is what one reflect run produced.
type Result struct {
	Text     string
	Evidence Evidence
	Usage    llm.Usage
	Turns    int
}

// Agent runs the reflect loop.
type Agent struct {
	Provider Provider
	Cfg      Config
}

// tools builds the OpenAI tool array for the loop.
func tools() []llm.Tool {
	fn := func(name, desc string, params map[string]any) llm.Tool {
		return llm.Tool{Type: "function", Function: llm.ToolDef{Name: name, Description: desc, Parameters: params}}
	}
	return []llm.Tool{
		fn(ToolRecall, "Search raw memories (facts and experiences). Ground-truth data the user actually said or did.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query string"},
			},
			"required": []string{"query"},
		}),
		fn(ToolSearchObservations, "Search consolidated observations (auto-synthesized knowledge, markdown).", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query string"},
			},
			"required": []string{"query"},
		}),
		fn(ToolDone, "Signal completion with your final answer. Use when you have enough information.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answer": map[string]any{
					"type":        "string",
					"description": "Your response as well-formatted markdown. NEVER include memory IDs in this text.",
				},
				"memory_ids":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Memory IDs supporting the answer"},
				"observation_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Observation IDs supporting the answer"},
			},
			"required": []string{"answer"},
		}),
	}
}

// Run executes the agent loop.
func (a *Agent) Run(ctx context.Context, src MemorySource, bankID, query string) (*Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("query is required")
	}
	cfg := a.Cfg
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 6
	}
	if cfg.Budget <= 0 {
		cfg.Budget = 20
	}

	mission, err := src.BankMission(ctx, bankID)
	if err != nil {
		return nil, err
	}
	directives, err := src.ListDirectives(ctx, bankID)
	if err != nil {
		return nil, err
	}

	system := buildSystemPrompt(mission, directives)
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: query},
	}

	ev := Evidence{Directives: directives}
	out := &Result{}

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		out.Turns = turn + 1
		resp, err := a.Provider.Chat(ctx, llm.ChatRequest{
			Model:       cfg.Model,
			Messages:    messages,
			Tools:       tools(),
			ToolChoice:  "auto",
			Temperature: cfg.Temperature,
			MaxTokens:   cfg.MaxTokens,
		})
		if err != nil {
			return nil, fmt.Errorf("turn %d: %w", turn+1, err)
		}
		out.Usage.PromptTokens += resp.Usage.PromptTokens
		out.Usage.CompletionTokens += resp.Usage.CompletionTokens
		out.Usage.TotalTokens += resp.Usage.TotalTokens

		choice := resp.Choices[0]
		assistant := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   choice.Message.Content,
			ToolCalls: choice.Message.ToolCalls,
		}
		messages = append(messages, assistant)

		if len(choice.Message.ToolCalls) == 0 {
			// model answered without tools: accept it as the final text
			out.Text = strings.TrimSpace(choice.Message.Content)
			if out.Text == "" {
				return nil, errors.New("model returned empty answer with no tool calls")
			}
			return out, nil
		}

		// execute each tool call in order, feed results back
		for _, tc := range choice.Message.ToolCalls {
			payload, done, terr := a.execTool(ctx, src, bankID, tc, &ev, cfg)
			if terr != nil {
				return nil, terr
			}
			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    payload,
			})
			if done {
				var doneOut struct {
					Answer string `json:"answer"`
				}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &doneOut); err == nil {
					out.Text = strings.TrimSpace(doneOut.Answer)
				}
				if out.Text == "" {
					out.Text = payload
				}
				out.Evidence = ev
				return out, nil
			}
		}
	}
	return nil, fmt.Errorf("reflect exceeded max turns (%d) without calling done", cfg.MaxTurns)
}

// execTool runs one tool call; done=true short-circuits the loop.
func (a *Agent) execTool(ctx context.Context, src MemorySource, bankID string, tc llm.ToolCall, ev *Evidence, cfg Config) (payload string, done bool, err error) {
	var args struct {
		Query string `json:"query"`
	}
	if tc.Function.Name != ToolDone {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return "", false, fmt.Errorf("tool %s: bad arguments: %w", tc.Function.Name, err)
		}
	}
	switch tc.Function.Name {
	case ToolRecall:
		hits, err := src.Recall(ctx, bankID, model.RecallOptions{
			Query: args.Query, Budget: cfg.Budget,
			Types: []model.FactType{model.FactWorld, model.FactExperience},
		})
		if err != nil {
			return "", false, err
		}
		for _, h := range hits {
			ev.Memories = append(ev.Memories, *h.Unit)
		}
		return renderToolResults("recall", hits), false, nil

	case ToolSearchObservations:
		hits, err := src.Recall(ctx, bankID, model.RecallOptions{
			Query: args.Query, Budget: cfg.Budget,
			Types: []model.FactType{model.FactObservation},
		})
		if err != nil {
			return "", false, err
		}
		for _, h := range hits {
			ev.MentalModels = append(ev.MentalModels, *h.Unit)
		}
		return renderToolResults("observations", hits), false, nil

	case ToolDone:
		// the payload is the raw arguments; the caller parses the answer
		return tc.Function.Arguments, true, nil

	default:
		return fmt.Sprintf(`{"error":"unknown tool %s"}`, tc.Function.Name), false, nil
	}
}

// renderToolResults serializes hits the way the upstream presenter does:
// short, id-tagged markdown blocks the model can cite.
func renderToolResults(kind string, hits []model.RecallHit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("[no %s found]", kind)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d %s:\n", len(hits), kind)
	for _, h := range hits {
		fmt.Fprintf(&b, "\n[id: %s] %s\n", h.Unit.ID, h.Unit.Text)
		if h.Unit.Context != "" {
			fmt.Fprintf(&b, "context: %s\n", h.Unit.Context)
		}
	}
	return b.String()
}

// buildSystemPrompt mirrors the upstream sections lite keeps: role, current
// behavior contract, mission, directives.
func buildSystemPrompt(mission string, directives []Directive) string {
	var b strings.Builder
	b.WriteString("You are a reflection agent that answers questions by reasoning over retrieved memories.\n\n")
	b.WriteString("## HOW TO WORK\n")
	b.WriteString("1. Use the recall tool to search raw memories, and search_observations for synthesized knowledge.\n")
	b.WriteString("2. When you have enough information, call the done tool with your answer as markdown.\n")
	b.WriteString("3. NEVER invent memories. Only cite what tools returned. If nothing was found, say so honestly.\n")
	b.WriteString("4. Put memory IDs only in the done tool's memory_ids/observation_ids arrays, never in the answer text.\n")
	if mission != "" {
		b.WriteString("\n## MISSION\n")
		b.WriteString(mission)
		b.WriteString("\n")
	}
	if len(directives) > 0 {
		b.WriteString("\n## DIRECTIVES (MANDATORY)\n")
		b.WriteString("These are hard rules you MUST follow in ALL responses:\n\n")
		for _, d := range directives {
			if d.Name != "" {
				fmt.Fprintf(&b, "- **%s**: %s\n", d.Name, d.Content)
			} else {
				fmt.Fprintf(&b, "- %s\n", d.Content)
			}
		}
		b.WriteString("\nNEVER violate these directives. Follow them silently; do not explain how.\n")
	}
	return b.String()
}
