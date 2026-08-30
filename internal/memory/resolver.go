package memory

import (
	"strings"
	"unicode"
)

type ResolutionAction string

const (
	ResolutionDuplicate ResolutionAction = "duplicate"
	ResolutionSupersede ResolutionAction = "supersede"
	ResolutionConflict  ResolutionAction = "conflict"
	ResolutionCoexist   ResolutionAction = "coexist"
)

type Resolution struct {
	Action     ResolutionAction
	RelatedIDs []string
	Reason     string
}

// ResolveCandidate is intentionally conservative. Structured memories only
// replace an existing value when extraction captured an explicit replacement
// signal; otherwise different values in the same slot become conflicts.
func ResolveCandidate(existing []Memory, candidate Candidate) Resolution {
	candidateContent := normalizeText(candidate.Content)
	candidateSubject := normalizeKey(candidate.Subject)
	candidateAttribute := normalizeKey(candidate.Attribute)
	candidateValue := normalizeText(candidate.Value)
	if candidateValue == "" {
		candidateValue = candidateContent
	}

	related := make([]Memory, 0, len(existing))
	for _, item := range existing {
		if item.Status != StatusActive && item.Status != StatusConflict {
			continue
		}
		if item.Kind != candidate.Kind {
			continue
		}
		if normalizeKey(item.Subject) != candidateSubject {
			continue
		}
		if disjointValidity(item, candidate) {
			continue
		}

		itemContent := normalizeText(item.Content)
		itemAttribute := normalizeKey(item.Attribute)
		itemValue := normalizeText(item.Value)
		if itemValue == "" {
			itemValue = itemContent
		}
		if itemAttribute == candidateAttribute && itemValue == candidateValue {
			return Resolution{Action: ResolutionDuplicate, RelatedIDs: []string{item.ID}, Reason: "same normalized subject, attribute, and value"}
		}
		// Legacy unstructured memories can only be deduplicated by normalized
		// text. Without an attribute key, treating similar prose as a conflict
		// would create false positives across unrelated facts.
		if candidateAttribute == "" || itemAttribute == "" {
			if itemContent == candidateContent {
				return Resolution{Action: ResolutionDuplicate, RelatedIDs: []string{item.ID}, Reason: "same normalized content"}
			}
			continue
		}
		if itemAttribute == candidateAttribute {
			related = append(related, item)
		}
	}

	if len(related) == 0 || normalizeChangeHint(candidate.ChangeHint) == ChangeHintCoexist {
		return Resolution{Action: ResolutionCoexist, Reason: "no conflicting structured memory"}
	}
	ids := make([]string, 0, len(related))
	for _, item := range related {
		ids = append(ids, item.ID)
	}
	if normalizeChangeHint(candidate.ChangeHint) == ChangeHintReplace {
		return Resolution{Action: ResolutionSupersede, RelatedIDs: ids, Reason: "explicit replacement of the same subject and attribute"}
	}
	return Resolution{Action: ResolutionConflict, RelatedIDs: ids, Reason: "different values for the same subject and attribute without a clear replacement"}
}

// SanitizeChangeHint prevents a model from silently replacing a fact when the
// source message contains no explicit change signal. A replacement is a
// destructive state transition; an uncertain hint is downgraded to conflict.
func SanitizeChangeHint(candidate Candidate, sourceText string) Candidate {
	if normalizeChangeHint(candidate.ChangeHint) == ChangeHintReplace && !explicitReplacementSignal(sourceText) {
		candidate.ChangeHint = ChangeHintUnknown
	}
	return candidate
}

func explicitReplacementSignal(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return false
	}
	for _, signal := range []string{
		"搬到", "搬去", "搬了", "改成", "改为", "换成", "换为", "变成", "变为",
		"现在住", "现在是", "不再", "已经改", "改回",
		"moved", "move to", "changed to", "change to", "now live", "no longer", "instead",
	} {
		if strings.Contains(text, signal) {
			return true
		}
	}
	if strings.Contains(text, "从") && strings.Contains(text, "到") {
		return true
	}
	return false
}

func disjointValidity(item Memory, candidate Candidate) bool {
	if !candidate.ValidFrom.IsZero() && !item.ExpiresAt.IsZero() && !candidate.ValidFrom.Before(item.ExpiresAt) {
		return true
	}
	if !item.ValidFrom.IsZero() && !candidate.ExpiresAt.IsZero() && !item.ValidFrom.Before(candidate.ExpiresAt) {
		return true
	}
	return false
}

func normalizeKey(value string) string {
	return strings.ToLower(normalizeText(value))
}

func normalizeText(value string) string {
	return strings.Join(strings.FieldsFunc(strings.TrimSpace(value), unicode.IsSpace), " ")
}

func normalizeChangeHint(value string) string {
	switch normalizeKey(value) {
	case ChangeHintReplace:
		return ChangeHintReplace
	case ChangeHintCoexist:
		return ChangeHintCoexist
	default:
		return ChangeHintUnknown
	}
}
