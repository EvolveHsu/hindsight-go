package server_test

import (
	"testing"

	"github.com/EvolveHsu/hindsight-go/internal/memory"
	"github.com/EvolveHsu/hindsight-go/internal/server"
	"github.com/EvolveHsu/hindsight-go/internal/storepg"
)

// TestMemoryReadSourceSeam pins the storage seam: both backends must satisfy
// server.MemoryReadSource. A silent type mismatch here degrades every
// memory-management endpoint to 501, which is exactly what happened before the
// DocumentInfo alias fix.
func TestMemoryReadSourceSeam(t *testing.T) {
	var memorySeam server.MemoryReadSource = memory.New()
	var pgSeam server.MemoryReadSource = (*storepg.Store)(nil)
	if memorySeam == nil || pgSeam == nil {
		t.Fatal("memory backend does not satisfy server.MemoryReadSource")
	}
}
