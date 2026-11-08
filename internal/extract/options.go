package extract

import (
	"fmt"
	"os"
	"strconv"
)

const (
	defaultMaxTokens      = 8192
	defaultMaxTokensLimit = 32768
	hardMaxTokensLimit    = 131072
	maxAttempts           = 3
)

// Options bounds the total number and size of extraction requests.
type Options struct {
	MaxTokens      int
	MaxTokensLimit int
}

func (o Options) normalized() (Options, error) {
	if o.MaxTokens == 0 {
		o.MaxTokens = defaultMaxTokens
	}
	if o.MaxTokensLimit == 0 {
		o.MaxTokensLimit = defaultMaxTokensLimit
	}
	if o.MaxTokens <= 0 || o.MaxTokensLimit < o.MaxTokens || o.MaxTokensLimit > hardMaxTokensLimit {
		return Options{}, fmt.Errorf("retain token budgets must satisfy 0 < initial <= limit <= %d", hardMaxTokensLimit)
	}
	return o, nil
}

// OptionsFromEnv validates extraction settings before the server starts.
func OptionsFromEnv() (Options, error) {
	o := Options{}
	for name, target := range map[string]*int{
		"HINDSIGHT_GO_RETAIN_MAX_TOKENS":       &o.MaxTokens,
		"HINDSIGHT_GO_RETAIN_MAX_TOKENS_LIMIT": &o.MaxTokensLimit,
	} {
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return Options{}, fmt.Errorf("%s must be a positive integer", name)
		}
		*target = value
	}
	return o.normalized()
}
