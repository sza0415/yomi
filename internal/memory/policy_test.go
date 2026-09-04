package memory

import "testing"

func TestPolicyRejectsSensitiveAndLowConfidenceCandidates(t *testing.T) {
	result := DefaultPolicy().Apply([]Candidate{
		{Kind: KindFact, Content: "用户密码是 abc123", Confidence: 0.99, Importance: 0.9},
		{Kind: KindFact, Content: "用户提供了一个凭证", Value: "api_key=abc123", Confidence: 0.99, Importance: 0.9},
		{Kind: KindFact, Content: "用户喜欢蓝色", Confidence: 0.2, Importance: 0.8},
		{Kind: KindPreference, Content: "用户偏好中文回答", Confidence: 0.9, Importance: 0.8},
	})
	if len(result.Accepted) != 1 || result.Accepted[0].Content != "用户偏好中文回答" {
		t.Fatalf("accepted = %#v", result.Accepted)
	}
	if result.Rejected != 3 || result.Reasons["sensitive_data"] != 2 || result.Reasons["low_confidence"] != 1 {
		t.Fatalf("rejected=%d reasons=%#v", result.Rejected, result.Reasons)
	}
}
