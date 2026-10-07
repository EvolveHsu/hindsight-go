package server

import _ "embed"

// bankTemplateVersion is the manifest schema version this build reads and
// writes (upstream BANK_TEMPLATE_CURRENT_VERSION).
const bankTemplateVersion = "1"

// bankTemplateSchema is the JSON Schema served by get_bank_template_schema.
// It is copied verbatim from the upstream docs site source of truth
// (hindsight-docs/static/bank-template-schema.json) so editors validate
// templates against exactly what the API accepts.
//
//go:embed bank_template_schema.json
var bankTemplateSchema []byte
