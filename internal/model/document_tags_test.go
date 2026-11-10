package model

import (
	"reflect"
	"testing"
)

func TestApplyDocumentTagPatch(t *testing.T) {
	current := []string{"keep", "remove", "keep"}
	got := ApplyDocumentTagPatch(current, nil, []string{"new", "keep"}, []string{"remove"})
	want := []string{"keep", "new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental patch = %v, want %v", got, want)
	}

	got = ApplyDocumentTagPatch(current, []string{"replace", "replace"}, []string{"ignored"}, nil)
	want = []string{"replace"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replacement patch = %v, want %v", got, want)
	}
}
