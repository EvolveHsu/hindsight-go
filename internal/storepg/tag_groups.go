package storepg

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/EvolveHsu/hindsight-go/internal/model"
)

// Compound tag filters (`tag_groups`) are a JSON-encoded list of recursive
// boolean expressions: leaves carry {tags, match, resolve}, internal nodes one
// of {and: [...]}, {or: [...]}, {not: {...}}, and the top-level groups AND-ed.
// Upstream resolves `resolve="fuzzy"` leaves against the bank's tag vocabulary
// with trigram similarity before building SQL; this build has no trigram
// resolver, so it rejects fuzzy leaves (422) instead of silently dropping the
// filter, and renders exact leaves directly.

type tagGroupNode struct {
	Tags    []string       `json:"tags"`
	Match   string         `json:"match"`
	Resolve string         `json:"resolve"`
	And     []tagGroupNode `json:"and"`
	Or      []tagGroupNode `json:"or"`
	Not     *tagGroupNode  `json:"not"`
}

func parseTagGroups(raw string) ([]tagGroupNode, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var nodes []tagGroupNode
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		return nil, fmt.Errorf("tag_groups: %w", err)
	}
	return nodes, nil
}

// memoryTagFilter renders the combined tags/tags_match/tag_groups predicate for
// a memory_units query whose $1 is the bank id. It returns the clause (no
// leading AND) plus the bind values after $1, in order.
func memoryTagFilter(tags []string, match, groupsRaw string) (string, []any, error) {
	conditions := []string{}
	params := []any{}
	next := 2
	if clause, bind := tagsMatchSQL("tags", tags, match, next); clause != "" {
		conditions = append(conditions, clause)
		if bind {
			params = append(params, tags)
			next++
		}
	}
	nodes, err := parseTagGroups(groupsRaw)
	if err != nil {
		return "", nil, err
	}
	if len(nodes) > 0 {
		clause, groupParams, nextIdx, err := buildTagGroupsClause(nodes, next)
		if err != nil {
			return "", nil, err
		}
		if clause != "" {
			conditions = append(conditions, clause)
			params = append(params, groupParams...)
			next = nextIdx
		}
	}
	return strings.Join(conditions, " AND "), params, nil
}

// entityTagFilterActive reports whether a compound or plain tag scope filters
// anything at all.
func entityTagFilterActive(tags []string, match, groupsRaw string) bool {
	if tagFilterActive(tags, match) {
		return true
	}
	nodes, err := parseTagGroups(groupsRaw)
	return err == nil && len(nodes) > 0
}

func buildTagGroupsClause(nodes []tagGroupNode, param int) (string, []any, int, error) {
	var clauses []string
	var params []any
	next := param
	for _, n := range nodes {
		clause, nodeParams, nextIdx, err := buildTagGroupNode(n, next)
		if err != nil {
			return "", nil, next, err
		}
		if clause == "" {
			continue
		}
		clauses = append(clauses, clause)
		params = append(params, nodeParams...)
		next = nextIdx
	}
	return strings.Join(clauses, " AND "), params, next, nil
}

func buildTagGroupNode(n tagGroupNode, param int) (string, []any, int, error) {
	switch {
	case n.Resolve == "fuzzy":
		return "", nil, param, model.ErrFuzzyTagGroups
	case len(n.And) > 0:
		return buildTagGroupGroup(n.And, "AND", param)
	case len(n.Or) > 0:
		return buildTagGroupGroup(n.Or, "OR", param)
	case n.Not != nil:
		clause, params, next, err := buildTagGroupNode(*n.Not, param)
		if err != nil {
			return "", nil, param, err
		}
		if clause == "" {
			return "", nil, param, nil
		}
		return "NOT (" + clause + ")", params, next, nil
	default:
		clause, bind := tagsMatchSQL("tags", n.Tags, n.Match, param)
		if clause == "" {
			return "", nil, param, nil
		}
		var params []any
		if bind {
			params = append(params, n.Tags)
			param++
		}
		return clause, params, param, nil
	}
}

func buildTagGroupGroup(children []tagGroupNode, op string, param int) (string, []any, int, error) {
	var clauses []string
	var params []any
	next := param
	for _, child := range children {
		clause, childParams, nextIdx, err := buildTagGroupNode(child, next)
		if err != nil {
			return "", nil, next, err
		}
		if clause == "" {
			continue
		}
		clauses = append(clauses, clause)
		params = append(params, childParams...)
		next = nextIdx
	}
	if len(clauses) == 0 {
		return "", nil, param, nil
	}
	return "(" + strings.Join(clauses, " "+op+" ") + ")", params, next, nil
}
