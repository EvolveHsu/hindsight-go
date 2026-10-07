package server

import (
	"sort"
	"strings"
	"time"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

func applyRecallRequestFilters(hits []model.RecallHit, req *api.RecallRequest) ([]model.RecallHit, error) {
	if req == nil {
		return hits, nil
	}
	if len(req.TagGroups) > 0 {
		return nil, badRequest("tag_groups are not supported by this build")
	}
	if len(req.Tags) > 0 || (req.TagsMatch.Set && req.TagsMatch.Value == "exact") {
		filtered := hits[:0]
		for _, hit := range hits {
			if recallTagsMatch(hit.Unit.Tags, req.Tags, string(req.TagsMatch.Value)) {
				filtered = append(filtered, hit)
			}
		}
		hits = filtered
	}
	if req.PreferObservations.Set && req.PreferObservations.Value {
		dropped := map[string]bool{}
		for _, hit := range hits {
			if hit.Unit.FactType == model.FactObservation {
				for _, id := range hit.Unit.SourceMemoryIds {
					dropped[id] = true
				}
			}
		}
		if len(dropped) > 0 {
			filtered := hits[:0]
			for _, hit := range hits {
				if hit.Unit.FactType != model.FactObservation && dropped[hit.Unit.ID] {
					continue
				}
				filtered = append(filtered, hit)
			}
			hits = filtered
		}
	}
	if req.MinScores.Set {
		finalFloor := 0.0
		rerankFloor := 0.0
		if req.MinScores.Value.Final.Set {
			finalFloor = req.MinScores.Value.Final.Value
		}
		if req.MinScores.Value.Reranker.Set {
			rerankFloor = req.MinScores.Value.Reranker.Value
		}
		if finalFloor > 0 || rerankFloor > 0 {
			filtered := hits[:0]
			for _, hit := range hits {
				if hit.FusedScore < finalFloor || hit.FusedScore < rerankFloor {
					continue
				}
				filtered = append(filtered, hit)
			}
			hits = filtered
		}
	}
	if req.TemporalWindow.Set {
		start, end := req.TemporalWindow.Value.Start, req.TemporalWindow.Value.End
		sort.SliceStable(hits, func(i, j int) bool {
			inI := recallInWindow(hits[i].Unit, start, end)
			inJ := recallInWindow(hits[j].Unit, start, end)
			if inI != inJ {
				return inI
			}
			return hits[i].FusedScore > hits[j].FusedScore
		})
	} else if req.QueryTimestamp.Set && req.QueryTimestamp.Value != "" {
		if anchor, err := time.Parse(time.RFC3339, req.QueryTimestamp.Value); err == nil {
			sort.SliceStable(hits, func(i, j int) bool {
				di := recallTimeDistance(hits[i].Unit, anchor)
				dj := recallTimeDistance(hits[j].Unit, anchor)
				if di != dj {
					return di < dj
				}
				return hits[i].FusedScore > hits[j].FusedScore
			})
		}
	}
	if req.MaxTokens.Set && req.MaxTokens.Value > 0 {
		budget := req.MaxTokens.Value * 4
		used := 0
		end := 0
		for _, hit := range hits {
			cost := len(hit.Unit.Text) + len(hit.Unit.Context)
			if used+cost > budget && end > 0 {
				break
			}
			used += cost
			end++
		}
		hits = hits[:end]
	}
	return hits, nil
}

func recallTagsMatch(tags, wanted []string, mode string) bool {
	has := func(tag string) bool {
		for _, t := range tags {
			if t == tag {
				return true
			}
		}
		return false
	}
	switch mode {
	case "all":
		for _, tag := range wanted {
			if !has(tag) {
				return false
			}
		}
		return true
	case "all_strict":
		if len(tags) == 0 {
			return false
		}
		for _, tag := range wanted {
			if !has(tag) {
				return false
			}
		}
		return true
	case "any_strict":
		if len(tags) == 0 {
			return false
		}
		for _, tag := range wanted {
			if has(tag) {
				return true
			}
		}
		return false
	case "exact":
		if len(tags) != len(wanted) {
			return false
		}
		for _, tag := range wanted {
			if !has(tag) {
				return false
			}
		}
		return true
	default: // any, including untagged rows
		for _, tag := range wanted {
			if has(tag) {
				return true
			}
		}
		return len(tags) == 0
	}
}

func recallInWindow(unit *model.Unit, start, end time.Time) bool {
	for _, raw := range []string{unit.MentionedAt, unit.CreatedAt} {
		if raw == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			if !ts.Before(start) && !ts.After(end) {
				return true
			}
		}
	}
	return false
}

func recallTimeDistance(unit *model.Unit, anchor time.Time) time.Duration {
	for _, raw := range []string{unit.MentionedAt, unit.CreatedAt} {
		if raw == "" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			if ts.After(anchor) {
				return ts.Sub(anchor)
			}
			return anchor.Sub(ts)
		}
	}
	return time.Duration(1<<62 - 1)
}

var _ = strings.TrimSpace
