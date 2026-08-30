package memory

import (
	"testing"
	"time"
)

func TestResolveCandidateUsesStructuredSlot(t *testing.T) {
	existing := []Memory{{
		ID: "mem-old", UserID: "alice", Kind: KindFact, Subject: "self",
		Attribute: "home_city", Value: "北京", Content: "用户住在北京", Status: StatusActive,
	}}

	tests := []struct {
		name      string
		candidate Candidate
		want      ResolutionAction
	}{
		{name: "duplicate", candidate: Candidate{Kind: KindFact, Subject: "SELF", Attribute: "home_city", Value: "北京", Content: "用户目前住在北京"}, want: ResolutionDuplicate},
		{name: "replace", candidate: Candidate{Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "上海", Content: "用户已搬到上海", ChangeHint: ChangeHintReplace}, want: ResolutionSupersede},
		{name: "conflict", candidate: Candidate{Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "上海", Content: "用户住在上海"}, want: ResolutionConflict},
		{name: "coexist", candidate: Candidate{Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "上海", Content: "用户也在上海居住", ChangeHint: ChangeHintCoexist}, want: ResolutionCoexist},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveCandidate(existing, tt.candidate)
			if got.Action != tt.want {
				t.Fatalf("action = %q, want %q (%s)", got.Action, tt.want, got.Reason)
			}
		})
	}
}

func TestResolveCandidateDoesNotConflictUnstructuredMemories(t *testing.T) {
	existing := []Memory{{ID: "mem-old", Kind: KindPreference, Content: "用户偏好中文回答", Status: StatusActive}}
	got := ResolveCandidate(existing, Candidate{Kind: KindPreference, Content: "用户喜欢简短回答"})
	if got.Action != ResolutionCoexist {
		t.Fatalf("action = %q, want coexist", got.Action)
	}
}

func TestSanitizeChangeHintRequiresExplicitReplacementSignal(t *testing.T) {
	candidate := Candidate{ChangeHint: ChangeHintReplace}
	got := SanitizeChangeHint(candidate, "我在上海有一套房")
	if got.ChangeHint != ChangeHintUnknown {
		t.Fatalf("change hint = %q, want unknown", got.ChangeHint)
	}
	got = SanitizeChangeHint(candidate, "我已经搬到上海了")
	if got.ChangeHint != ChangeHintReplace {
		t.Fatalf("change hint = %q, want replace", got.ChangeHint)
	}
	got = SanitizeChangeHint(candidate, "我从小喜欢北京")
	if got.ChangeHint != ChangeHintUnknown {
		t.Fatalf("change hint = %q, want unknown for unrelated 从", got.ChangeHint)
	}
}

func TestResolveCandidateSeparatesNonOverlappingValidity(t *testing.T) {
	old := Memory{ID: "mem-old", Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "北京", Content: "用户住在北京", Status: StatusActive, ExpiresAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	candidate := Candidate{Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "上海", Content: "用户住在上海", ValidFrom: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
	got := ResolveCandidate([]Memory{old}, candidate)
	if got.Action != ResolutionCoexist {
		t.Fatalf("action = %q, want coexist", got.Action)
	}
}
