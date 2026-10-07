package storepg

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const auditTimeLayout = `YYYY-MM-DD"T"HH24:MI:SSTZH:TZM`

type auditJSONRow struct {
	ID         string          `json:"id"`
	Action     string          `json:"action"`
	Transport  string          `json:"transport"`
	BankID     string          `json:"bank_id"`
	StartedAt  string          `json:"started_at"`
	EndedAt    string          `json:"ended_at"`
	DurationMs *int            `json:"duration_ms"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
	Metadata   json.RawMessage `json:"metadata"`
}

func (s *Store) AuditLogsJSON(ctx context.Context, bankID, action, transport, startDate, endDate string, limit, offset int) ([]byte, error) {
	if limit <= 0 {
		limit = 50
	}
	where := []string{"bank_id=$1"}
	args := []any{bankID}
	add := func(column, op, value string) {
		if value == "" {
			return
		}
		args = append(args, value)
		where = append(where, fmt.Sprintf("%s %s $%d", column, op, len(args)))
	}
	add("action", "=", action)
	add("transport", "=", transport)
	add("started_at", ">=", startDate)
	add("started_at", "<", endDate)
	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT id::text, action, transport, COALESCE(bank_id,''), to_char(started_at, '%s'),
       COALESCE(to_char(ended_at, '%s'),''),
       CASE WHEN ended_at IS NULL THEN NULL ELSE (EXTRACT(EPOCH FROM (ended_at-started_at))*1000)::int END,
       COALESCE(request::text,'null'), COALESCE(response::text,'null'), COALESCE(metadata::text,'{}')
FROM audit_log WHERE %s ORDER BY started_at DESC LIMIT $%d OFFSET $%d`,
		auditTimeLayout, auditTimeLayout, whereSQL, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []auditJSONRow{}
	for rows.Next() {
		var r auditJSONRow
		var duration *int
		var request, response, metadata string
		if err := rows.Scan(&r.ID, &r.Action, &r.Transport, &r.BankID, &r.StartedAt, &r.EndedAt,
			&duration, &request, &response, &metadata); err != nil {
			return nil, err
		}
		r.DurationMs = duration
		r.Request = json.RawMessage(request)
		r.Response = json.RawMessage(response)
		r.Metadata = json.RawMessage(metadata)
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"bank_id": bankID, "total": total, "limit": limit, "offset": offset, "items": items,
	})
}

func (s *Store) AuditStatsJSON(ctx context.Context, bankID, action, period string) ([]byte, error) {
	trunc := "day"
	interval := "7 days"
	switch period {
	case "1d":
		trunc, interval = "hour", "1 day"
	case "7d":
		trunc, interval = "day", "7 days"
	case "30d":
		trunc, interval = "day", "30 days"
	}
	args := []any{bankID, interval}
	actionFilter := ""
	if action != "" {
		args = append(args, action)
		actionFilter = " AND action=$3"
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT to_char(date_trunc('%s', started_at), '%s'), action, count(*)
FROM audit_log
WHERE bank_id=$1 AND started_at >= now() - $2::interval%s
GROUP BY 1,2 ORDER BY 1`, trunc, auditTimeLayout, actionFilter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type bucket struct {
		Time    string         `json:"time"`
		Actions map[string]int `json:"actions"`
		Total   int            `json:"total"`
	}
	byTime := map[string]*bucket{}
	for rows.Next() {
		var ts, act string
		var n int
		if err := rows.Scan(&ts, &act, &n); err != nil {
			return nil, err
		}
		b := byTime[ts]
		if b == nil {
			b = &bucket{Time: ts, Actions: map[string]int{}}
			byTime[ts] = b
		}
		b.Actions[act] = n
		b.Total += n
	}
	keys := make([]string, 0, len(byTime))
	for k := range byTime {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buckets := make([]bucket, 0, len(keys))
	for _, k := range keys {
		buckets = append(buckets, *byTime[k])
	}
	start := ""
	if len(buckets) > 0 {
		start = buckets[0].Time
	}
	return json.Marshal(map[string]any{
		"bank_id": bankID, "period": period, "trunc": trunc, "start": start, "buckets": buckets,
	})
}

type llmJSONRow struct {
	ID             string          `json:"id"`
	BankID         string          `json:"bank_id"`
	Operation      string          `json:"operation"`
	Scope          string          `json:"scope"`
	TraceID        string          `json:"trace_id"`
	SpanID         string          `json:"span_id"`
	ParentSpanID   string          `json:"parent_span_id"`
	Provider       string          `json:"provider"`
	Model          string          `json:"model"`
	Status         string          `json:"status"`
	StartedAt      string          `json:"started_at"`
	EndedAt        string          `json:"ended_at"`
	DurationMs     int             `json:"duration_ms"`
	InputTokens    int             `json:"input_tokens"`
	OutputTokens   int             `json:"output_tokens"`
	CachedTokens   int             `json:"cached_tokens"`
	ThoughtsTokens int             `json:"thoughts_tokens"`
	TotalTokens    int             `json:"total_tokens"`
	Input          json.RawMessage `json:"input"`
	Output         json.RawMessage `json:"output"`
	Error          string          `json:"error"`
	LLMInfo        json.RawMessage `json:"llm_info"`
	Metadata       json.RawMessage `json:"metadata"`
}

func (s *Store) LLMRequestsJSON(ctx context.Context, bankID, status, operation, scope, provider, traceID, startDate, endDate string, limit, offset int) ([]byte, error) {
	if limit <= 0 {
		limit = 50
	}
	where := []string{"bank_id=$1"}
	args := []any{bankID}
	add := func(column, op, value string) {
		if value == "" {
			return
		}
		args = append(args, value)
		where = append(where, fmt.Sprintf("%s %s $%d", column, op, len(args)))
	}
	add("status", "=", status)
	add("operation", "=", operation)
	add("scope", "=", scope)
	add("provider", "=", provider)
	add("trace_id", "=", traceID)
	add("started_at", ">=", startDate)
	add("started_at", "<", endDate)
	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM llm_requests WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT id::text, COALESCE(bank_id,''), COALESCE(operation,''), COALESCE(scope,''),
       COALESCE(trace_id,''), COALESCE(span_id,''), COALESCE(parent_span_id,''),
       COALESCE(provider,''), COALESCE(model,''), status,
       to_char(started_at, '%[1]s'), COALESCE(to_char(ended_at, '%[1]s'),''),
       COALESCE(duration_ms,0), COALESCE(input_tokens,0), COALESCE(output_tokens,0),
       COALESCE(cached_tokens,0), COALESCE(thoughts_tokens,0), COALESCE(total_tokens,0),
       COALESCE(input::text,'null'), COALESCE(output::text,'null'), COALESCE(error,''),
       COALESCE(llm_info::text,'{}'), COALESCE(metadata::text,'{}')
FROM llm_requests WHERE %[2]s ORDER BY started_at DESC LIMIT $%[3]d OFFSET $%[4]d`,
		auditTimeLayout, whereSQL, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []llmJSONRow{}
	for rows.Next() {
		var r llmJSONRow
		var input, output, info, metadata string
		if err := rows.Scan(&r.ID, &r.BankID, &r.Operation, &r.Scope, &r.TraceID, &r.SpanID,
			&r.ParentSpanID, &r.Provider, &r.Model, &r.Status, &r.StartedAt, &r.EndedAt,
			&r.DurationMs, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.ThoughtsTokens,
			&r.TotalTokens, &input, &output, &r.Error, &info, &metadata); err != nil {
			return nil, err
		}
		r.Input = json.RawMessage(input)
		r.Output = json.RawMessage(output)
		r.LLMInfo = json.RawMessage(info)
		r.Metadata = json.RawMessage(metadata)
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"bank_id": bankID, "total": total, "limit": limit, "offset": offset, "items": items,
	})
}

func (s *Store) LLMStatsJSON(ctx context.Context, bankID, operation, period string) ([]byte, error) {
	trunc := "day"
	interval := "7 days"
	switch period {
	case "1d":
		trunc, interval = "hour", "1 day"
	case "7d":
		trunc, interval = "day", "7 days"
	case "30d":
		trunc, interval = "day", "30 days"
	}
	args := []any{bankID, interval}
	opFilter := ""
	if operation != "" {
		args = append(args, operation)
		opFilter = " AND operation=$3"
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT to_char(date_trunc('%s', started_at), '%s'), status, count(*),
       COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0),
       COALESCE(sum(cached_tokens),0), COALESCE(sum(thoughts_tokens),0), COALESCE(sum(total_tokens),0)
FROM llm_requests
WHERE bank_id=$1 AND started_at >= now() - $2::interval%s
GROUP BY 1,2 ORDER BY 1`, trunc, auditTimeLayout, opFilter), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type tokens struct {
		Input    int `json:"input"`
		Output   int `json:"output"`
		Cached   int `json:"cached"`
		Thoughts int `json:"thoughts"`
		Total    int `json:"total"`
	}
	type bucket struct {
		Time     string         `json:"time"`
		Statuses map[string]int `json:"statuses"`
		Total    int            `json:"total"`
		Tokens   tokens         `json:"tokens"`
	}
	byTime := map[string]*bucket{}
	for rows.Next() {
		var ts, status string
		var n, in, out, cached, thoughts, total int
		if err := rows.Scan(&ts, &status, &n, &in, &out, &cached, &thoughts, &total); err != nil {
			return nil, err
		}
		b := byTime[ts]
		if b == nil {
			b = &bucket{Time: ts, Statuses: map[string]int{}}
			byTime[ts] = b
		}
		b.Statuses[status] = n
		b.Total += n
		b.Tokens.Input += in
		b.Tokens.Output += out
		b.Tokens.Cached += cached
		b.Tokens.Thoughts += thoughts
		b.Tokens.Total += total
	}
	keys := make([]string, 0, len(byTime))
	for k := range byTime {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buckets := make([]bucket, 0, len(keys))
	for _, k := range keys {
		buckets = append(buckets, *byTime[k])
	}
	start := ""
	if len(buckets) > 0 {
		start = buckets[0].Time
	}
	return json.Marshal(map[string]any{
		"bank_id": bankID, "period": period, "trunc": trunc, "start": start, "buckets": buckets,
	})
}

func (s *Store) MentalModelHistoryJSON(ctx context.Context, bankID, modelID string) ([]byte, error) {
	rows, err := s.pool.Query(ctx, `
SELECT COALESCE(content::text,'null'), to_char(changed_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
FROM mental_model_history WHERE bank_id=$1 AND mental_model_id=$2 ORDER BY changed_at`, bankID, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var content, changed string
		if err := rows.Scan(&content, &changed); err != nil {
			return nil, err
		}
		var decoded any
		_ = json.Unmarshal([]byte(content), &decoded)
		items = append(items, map[string]any{"content": decoded, "changed_at": changed})
	}
	return json.Marshal(items)
}

func (s *Store) ObservationHistoryJSON(ctx context.Context, bankID, observationID string) ([]byte, error) {
	rows, err := s.pool.Query(ctx, `
SELECT COALESCE(content::text,'null'), to_char(changed_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
FROM observation_history WHERE bank_id=$1 AND observation_id::text=$2 ORDER BY changed_at`, bankID, observationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var content, changed string
		if err := rows.Scan(&content, &changed); err != nil {
			return nil, err
		}
		var decoded any
		_ = json.Unmarshal([]byte(content), &decoded)
		items = append(items, map[string]any{"content": decoded, "changed_at": changed})
	}
	return json.Marshal(items)
}

func (s *Store) BankExists(ctx context.Context, bankID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM banks WHERE bank_id=$1)`, bankID).Scan(&exists)
	return exists, err
}
