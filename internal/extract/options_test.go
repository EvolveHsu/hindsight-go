package extract

import (
	"strconv"
	"testing"
)

func TestOptionsFromEnv(t *testing.T) {
	for _, tt := range []struct {
		initial, limit, context string
		want                    Options
		wantErr                 bool
	}{
		{"", "", "", Options{MaxTokens: 8192, MaxTokensLimit: 32768, ContextTokens: 1000000}, false},
		{"2048", "8192", "1000000", Options{MaxTokens: 2048, MaxTokensLimit: 8192, ContextTokens: 1000000}, false},
		{"131072", "131072", "1000000", Options{MaxTokens: 131072, MaxTokensLimit: 131072, ContextTokens: 1000000}, false},
		{"0", "", "", Options{}, true},
		{"-1", "", "", Options{}, true},
		{"abc", "", "", Options{}, true},
		{"8192", "4096", "", Options{}, true},
		{"", "0", "", Options{}, true},
		{"", "131073", "", Options{}, true},
		{"", "", "8192", Options{}, true},
		{"999999999999999999999999999999999999", "", "", Options{}, true},
	} {
		t.Run(tt.initial+"/"+tt.limit+"/"+tt.context, func(t *testing.T) {
			t.Setenv("HINDSIGHT_GO_RETAIN_MAX_TOKENS", tt.initial)
			t.Setenv("HINDSIGHT_GO_RETAIN_MAX_TOKENS_LIMIT", tt.limit)
			t.Setenv("HINDSIGHT_GO_LLM_CONTEXT_TOKENS", tt.context)
			got, err := OptionsFromEnv()
			if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}

func TestInvalidOptions(t *testing.T) {
	for i, o := range []Options{
		{MaxTokens: -1, MaxTokensLimit: 8192},
		{MaxTokens: 8192, MaxTokensLimit: -1},
		{MaxTokens: 8192, MaxTokensLimit: 4096},
		{MaxTokens: 8192, MaxTokensLimit: 131073},
		{MaxTokens: 8192, MaxTokensLimit: 32768, ContextTokens: 8192},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if _, err := NewWithOptions(nil, "", o); err == nil {
				t.Fatalf("accepted %+v", o)
			}
		})
	}
}
