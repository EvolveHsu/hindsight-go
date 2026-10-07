package model

// BankStats is the aggregate used by GET /banks/{id}/stats.
type BankStats struct {
	TotalNodes           int
	TotalLinks           int
	TotalDocuments       int
	CreatedAt            string
	UpdatedAt            string
	LastDocumentAt       string
	NodesByFactType      map[string]int
	LinksByLinkType      map[string]int
	OperationsByStatus   map[string]int
	LastConsolidatedAt   string
	LastMemoryWriteAt    string
	PendingConsolidation int
	FailedConsolidation  int
	TotalObservations    int
}

// MemoryBucket is one bucket in the memories-timeseries endpoint.
type MemoryBucket struct {
	Time        string
	World       int
	Experience  int
	Observation int
}

// ScopeCount is one distinct observation scope and its row count.
type ScopeCount struct {
	Tags  []string
	Count int
}
