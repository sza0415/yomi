package tools

import (
	"context"
	"testing"
)

type permissionTestAsker struct{ answer string }

func (a permissionTestAsker) Ask(context.Context, string, []string) (string, error) {
	return a.answer, nil
}

type optionsPermissionAsker struct {
	answer  string
	options []string
}

func (a *optionsPermissionAsker) Ask(_ context.Context, _ string, options []string) (string, error) {
	a.options = append([]string(nil), options...)
	return a.answer, nil
}

type countingPermissionAsker struct{ calls int }

func (a *countingPermissionAsker) Ask(context.Context, string, []string) (string, error) {
	a.calls++
	return "Allow always", nil
}

type sequencePermissionAsker struct {
	answers []string
	calls   int
}

func (a *sequencePermissionAsker) Ask(context.Context, string, []string) (string, error) {
	answer := a.answers[a.calls]
	a.calls++
	return answer, nil
}

func TestPolicyGateReadOnlyToolAllowed(t *testing.T) {
	gate := NewPolicyGate(PermissionSafe)
	if err := gate.Check(context.Background(), PermissionRequest{Tool: "read_file"}); err != nil {
		t.Fatalf("read-only check error = %v", err)
	}
}

func TestPolicyGateDeniesWithoutInteractiveApproval(t *testing.T) {
	gate := NewPolicyGate(PermissionSafe)
	if err := gate.Check(context.Background(), PermissionRequest{Tool: "bash", Reason: "runs commands"}); err == nil {
		t.Fatal("bash without asker should be denied")
	}
}

func TestPolicyGateRequiresAndAcceptsApproval(t *testing.T) {
	gate := NewPolicyGate(PermissionSafe)
	ctx := WithAsker(context.Background(), permissionTestAsker{answer: "Allow once"})
	if err := gate.Check(ctx, PermissionRequest{Tool: "write_file", Reason: "modifies files"}); err != nil {
		t.Fatalf("approved write check error = %v", err)
	}
}

func TestPolicyGateOffersAllowAlwaysOption(t *testing.T) {
	gate := NewPolicyGate(PermissionSafe)
	asker := &optionsPermissionAsker{answer: "Allow once"}
	ctx := WithAsker(context.Background(), asker)
	if err := gate.Check(ctx, PermissionRequest{Tool: "web_search"}); err != nil {
		t.Fatalf("web_search approval error = %v", err)
	}
	want := []string{"Allow once", "Allow always", "Deny"}
	if len(asker.options) != len(want) {
		t.Fatalf("options = %#v, want %#v", asker.options, want)
	}
	for i := range want {
		if asker.options[i] != want[i] {
			t.Fatalf("options = %#v, want %#v", asker.options, want)
		}
	}
}

func TestPolicyGateAllowAlwaysSkipsSubsequentApproval(t *testing.T) {
	gate := NewPolicyGate(PermissionSafe)
	asker := &countingPermissionAsker{}
	ctx := WithAsker(context.Background(), asker)
	for i := 0; i < 2; i++ {
		if err := gate.Check(ctx, PermissionRequest{Tool: "bash", Reason: "runs commands"}); err != nil {
			t.Fatalf("allow always check %d error = %v", i, err)
		}
	}
	if asker.calls != 1 {
		t.Fatalf("asker calls = %d, want 1", asker.calls)
	}
}

func TestPolicyGateAllowAlwaysIsPerTool(t *testing.T) {
	gate := NewPolicyGate(PermissionSafe)
	asker := &sequencePermissionAsker{answers: []string{"Allow always", "Deny"}}
	ctx := WithAsker(context.Background(), asker)
	if err := gate.Check(ctx, PermissionRequest{Tool: "python"}); err != nil {
		t.Fatalf("python approval error = %v", err)
	}
	if err := gate.Check(ctx, PermissionRequest{Tool: "web_fetch"}); err == nil {
		t.Fatal("web_fetch should require its own approval")
	}
	if asker.calls != 2 {
		t.Fatalf("asker calls = %d, want 2", asker.calls)
	}
}
