package memory

import (
	"regexp"
	"strings"
)

type Policy struct {
	MinConfidence   float64
	MinImportance   float64
	MaxContentChars int
}

type PolicyResult struct {
	Accepted []Candidate
	Rejected int
	Reasons  map[string]int
}

func DefaultPolicy() Policy {
	return Policy{MinConfidence: 0.65, MinImportance: 0.20, MaxContentChars: 1000}
}

func (p Policy) Apply(candidates []Candidate) PolicyResult {
	if p.MinConfidence <= 0 {
		p.MinConfidence = 0.65
	}
	if p.MinImportance <= 0 {
		p.MinImportance = 0.20
	}
	if p.MaxContentChars <= 0 {
		p.MaxContentChars = 1000
	}
	result := PolicyResult{Reasons: make(map[string]int)}
	for _, candidate := range candidates {
		reason := ""
		switch {
		case strings.TrimSpace(candidate.Content) == "":
			reason = "empty_content"
		case candidate.Kind != KindFact && candidate.Kind != KindPreference && candidate.Kind != KindEpisode:
			reason = "unsupported_kind"
		case candidate.Confidence < p.MinConfidence:
			reason = "low_confidence"
		case candidate.Importance < p.MinImportance:
			reason = "low_importance"
		case len([]rune(candidate.Content)) > p.MaxContentChars:
			reason = "content_too_long"
		case sensitivePattern.MatchString(candidate.Content) || sensitivePattern.MatchString(candidate.Evidence):
			reason = "sensitive_data"
		}
		if reason != "" {
			result.Rejected++
			result.Reasons[reason]++
			continue
		}
		result.Accepted = append(result.Accepted, candidate)
	}
	return result
}

var sensitivePattern = regexp.MustCompile(`(?i)(password|passwd|secret|api[_ -]?key|access[_ -]?token|private[_ -]?key|信用卡|银行卡|密码|密钥|口令)|\b\d{13,19}\b`)
