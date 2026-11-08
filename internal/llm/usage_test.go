package llm

import (
	"encoding/json"
	"testing"
)

func TestReasoningTokenUsage(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want int
	}{
		{`{}`, 0},
		{`{"completion_thinking_tokens":1866}`, 1866},
		{`{"completion_tokens_details":{"reasoning_tokens":3923}}`, 3923},
		{`{"completion_thinking_tokens":1866,"completion_tokens_details":{"reasoning_tokens":3923}}`, 3923},
	} {
		var u Usage
		if err := json.Unmarshal([]byte(tt.raw), &u); err != nil {
			t.Fatal(err)
		}
		if u.ReasoningTokens() != tt.want {
			t.Fatalf("reasoning tokens=%d want=%d", u.ReasoningTokens(), tt.want)
		}
	}
}
