package extract

import (
	"strconv"
	"testing"
)

func TestOptionsFromEnv(t *testing.T) {
	for _, tt := range []struct {
		initial, limit string
		want           Options
		wantErr        bool
	}{
		{"", "", Options{8192, 32768}, false},
		{"2048", "8192", Options{2048, 8192}, false},
		{"131072", "131072", Options{131072, 131072}, false},
		{"0", "", Options{}, true},
		{"-1", "", Options{}, true},
		{"abc", "", Options{}, true},
		{"8192", "4096", Options{}, true},
		{"", "0", Options{}, true},
		{"", "131073", Options{}, true},
		{"999999999999999999999999999999999999", "", Options{}, true},
	} {
		t.Run(tt.initial+"/"+tt.limit, func(t *testing.T) {
			t.Setenv("HINDSIGHT_GO_RETAIN_MAX_TOKENS", tt.initial)
			t.Setenv("HINDSIGHT_GO_RETAIN_MAX_TOKENS_LIMIT", tt.limit)
			got, err := OptionsFromEnv()
			if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}

func TestInvalidOptions(t *testing.T) {
	for i, o := range []Options{{-1, 8192}, {8192, -1}, {8192, 4096}, {8192, 131073}} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if _, err := NewWithOptions(nil, "", o); err == nil {
				t.Fatalf("accepted %+v", o)
			}
		})
	}
}
