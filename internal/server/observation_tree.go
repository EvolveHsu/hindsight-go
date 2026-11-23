package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	api "github.com/EvolveHsu/hindsight-go/internal/api"
	"github.com/EvolveHsu/hindsight-go/internal/model"
)

type observationTreeSource interface {
	Observations(ctx context.Context, bankID string, limit int) ([]model.Unit, error)
}

// GetObservationTree implements get_observation_tree. The tree is a
// deterministic, read-only view over consolidated observations; it does not
// create a second persistence model.
func (e *Engine) GetObservationTree(ctx context.Context, params api.GetObservationTreeParams) (api.GetObservationTreeRes, error) {
	if b, err := e.store.GetBank(ctx, params.BankID); err != nil {
		return nil, internal(err)
	} else if b == nil {
		return nil, notFound("bank %q does not exist", params.BankID)
	}
	if len(params.Tags) > 0 && params.TagGroups.Set && strings.TrimSpace(params.TagGroups.Value) != "" {
		return nil, badRequest("tags and tag_groups are mutually exclusive")
	}
	src, ok := e.store.(observationTreeSource)
	if !ok {
		return nil, &statusError{
			status: http.StatusNotImplemented,
			body:   map[string]any{"detail": "backend does not support the observation tree"},
		}
	}
	units, err := src.Observations(ctx, params.BankID, 1_000_000)
	if err != nil {
		return nil, internal(err)
	}
	match := "any"
	if params.TagsMatch.Set {
		match = string(params.TagsMatch.Value)
	}
	groups := ""
	if params.TagGroups.Set {
		groups = params.TagGroups.Value
	}
	filtered := units[:0]
	for _, unit := range units {
		if len(params.Tags) == 0 && groups == "" || knowledgeTagsMatch(unit.Tags, params.Tags, match, groups) {
			filtered = append(filtered, unit)
		}
	}
	units = filtered

	grouped := map[string][]model.Unit{}
	for _, unit := range units {
		key := "observations"
		if len(unit.Tags) > 0 {
			key = unit.Tags[0]
		}
		grouped[key] = append(grouped[key], unit)
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	usedPaths := map[string]bool{}
	files := make([]api.ObservationTreeFile, 0, len(keys))
	for _, key := range keys {
		path := uniqueObservationPath(key, usedPaths)
		files = append(files, observationTreeFile(key, path, grouped[key]))
	}
	return &api.ObservationTreeFolder{
		Path:             "",
		Name:             "",
		Title:            "Observations",
		ObservationCount: len(units),
		Folders:          []api.ObservationTreeFolder{},
		Files:            files,
	}, nil
}

func observationTreeFile(tag, path string, units []model.Unit) api.ObservationTreeFile {
	observations := make([]api.ObservationTreeObservation, 0, len(units))
	for _, unit := range units {
		observations = append(observations, api.ObservationTreeObservation{
			ID:            unit.ID,
			Text:          unit.Text,
			ProofCount:    len(unit.SourceMemoryIds),
			OccurredStart: optObservationTime(unit.OccurredStart),
			OccurredEnd:   optObservationTime(unit.OccurredEnd),
			MentionedAt:   optObservationTime(unit.MentionedAt),
			UpdatedAt:     optObservationTime(unit.CreatedAt),
			Tags:          normalizeTags(unit.Tags),
		})
	}
	about := []string{}
	if tag != "observations" {
		about = []string{tag}
	}
	title := observationTitle(tag)
	return api.ObservationTreeFile{
		Path:             path,
		Name:             path,
		Kind:             api.ObservationTreeFileKindTopic,
		Title:            title,
		ObservationCount: len(observations),
		About:            about,
		Related:          []string{},
		Observations:     observations,
		Content:          observationMarkdown(title, observations),
	}
}

func observationMarkdown(title string, observations []api.ObservationTreeObservation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	for _, observation := range observations {
		fmt.Fprintf(&b, "- %s", observation.Text)
		if observation.ProofCount > 0 {
			fmt.Fprintf(&b, " (%d sources)", observation.ProofCount)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func observationTitle(tag string) string {
	if tag == "observations" {
		return "Observations"
	}
	parts := strings.FieldsFunc(tag, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	if len(parts) == 0 {
		return "Observations"
	}
	return strings.Join(parts, " ")
}

func uniqueObservationPath(tag string, used map[string]bool) string {
	slug := observationSlug(tag)
	if slug == "" {
		slug = "observations"
	}
	path := slug + ".md"
	for n := 2; used[path]; n++ {
		path = fmt.Sprintf("%s-%d.md", slug, n)
	}
	used[path] = true
	return path
}

func observationSlug(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func optObservationTime(value string) api.OptDateTime {
	if strings.TrimSpace(value) == "" {
		return api.OptDateTime{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-07"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return api.OptDateTime{Set: true, Value: parsed}
		}
	}
	return api.OptDateTime{}
}
