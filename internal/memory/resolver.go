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

// ResolveCandidate 有意采用保守策略。只有提取阶段捕获到明确的替换信号时，
// 结构化记忆才会替换已有值；否则，同一位置上的不同值会被标记为冲突。
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
		// 旧版非结构化记忆只能根据规范化后的文本去重。如果没有属性键，
		// 把相似表述视为冲突会在互不相关的事实之间产生误判。
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

// NeedsReplacementConfirmation reports whether a provider-proposed replacement
// lacks direct support in the user's source text. Callers should only ask the
// user after ResolveCandidate has confirmed that an existing memory would
// actually be superseded.
func NeedsReplacementConfirmation(candidate Candidate, sourceText string) bool {
	return normalizeChangeHint(candidate.ChangeHint) == ChangeHintReplace && !explicitReplacementSignal(sourceText)
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
