package model

// ApplyDocumentTagPatch applies the v1.2.1 document tag update modes.
// A non-nil replace means full replacement; otherwise removals run before
// additions. The result is deduplicated while preserving the original order.
func ApplyDocumentTagPatch(current, replace, add, remove []string) []string {
	if replace != nil {
		return dedupeTags(replace)
	}
	removeSet := make(map[string]struct{}, len(remove))
	for _, tag := range remove {
		removeSet[tag] = struct{}{}
	}
	out := make([]string, 0, len(current)+len(add))
	seen := make(map[string]struct{}, len(current)+len(add))
	for _, tag := range current {
		if _, removed := removeSet[tag]; removed {
			continue
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	for _, tag := range add {
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func dedupeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}
