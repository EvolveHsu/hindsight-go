package model

// KnowledgeRow is the storage projection for a knowledge-base folder or page.
// It intentionally carries the mental-model fields the page API returns; the
// backing tables remain upstream-owned.
type KnowledgeRow struct {
	ID                  string
	BankID              string
	ParentID            string
	Kind                string
	Name                string
	MentalModelID       string
	Managed             bool
	SortOrder           int
	Description         string
	Tags                []string
	Body                string
	LastRefreshedAt     string
	LastRefreshFailedAt string
	CreatedAt           string
	UpdatedAt           string
}

// KnowledgePatch is a sparse update for a knowledge node or its backing
// mental model. Nil fields are left unchanged.
type KnowledgePatch struct {
	Name        *string
	ParentID    *string
	SourceQuery *string
	Tags        *[]string
	MaxTokens   *int
	Trigger     []byte
}

// WebhookRow is the storage projection for an upstream webhook row.
type WebhookRow struct {
	ID         string
	BankID     string
	URL        string
	Secret     string
	EventTypes []string
	Enabled    bool
	HTTPConfig []byte
	CreatedAt  string
	UpdatedAt  string
}

// WebhookPatch is a sparse webhook update. Nil fields are left unchanged.
type WebhookPatch struct {
	URL           *string
	Secret        *string
	SecretPresent bool
	EventTypes    *[]string
	Enabled       *bool
	HTTPConfig    []byte
}

// WebhookDeliveryRow is a webhook_delivery async operation projection.
type WebhookDeliveryRow struct {
	ID                 string
	WebhookID          string
	URL                string
	EventType          string
	Status             string
	Attempts           int
	NextRetryAt        string
	LastError          string
	LastResponseStatus int
	LastResponseBody   string
	LastAttemptAt      string
	CreatedAt          string
	UpdatedAt          string
}
