package agent

import "testing"

func TestRunTransitionValidLifecycle(t *testing.T) {
	run := NewRun("s1", RunBudget{})
	if err := run.Transition(RunRunning, "started"); err != nil {
		t.Fatal(err)
	}
	if err := run.Transition(RunWaitingUser, "needs answer"); err != nil {
		t.Fatal(err)
	}
	if err := run.Transition(RunRunning, "answered"); err != nil {
		t.Fatal(err)
	}
	if err := run.Transition(RunCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if run.Status != RunCompleted || run.StartedAt.IsZero() || run.FinishedAt.IsZero() {
		t.Fatalf("run lifecycle = %#v", run)
	}
	if run.Error != "" || run.StatusReason != "done" {
		t.Fatalf("run completion metadata = %#v", run)
	}
}

func TestRunTransitionRejectsTerminalAndInvalidStates(t *testing.T) {
	run := NewRun("s1", RunBudget{})
	if err := run.Transition(RunCompleted, "skipping execution"); err == nil {
		t.Fatal("expected queued -> completed to be rejected")
	}
	if err := run.Transition(RunRunning, "started"); err != nil {
		t.Fatal(err)
	}
	if err := run.Transition(RunCancelled, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := run.Transition(RunRunning, "must remain terminal"); err == nil {
		t.Fatal("expected terminal -> running to be rejected")
	}
}

func TestRunTransitionAllowsIdempotentState(t *testing.T) {
	run := NewRun("s1", RunBudget{})
	if err := run.Transition(RunRunning, "started"); err != nil {
		t.Fatal(err)
	}
	if err := run.Transition(RunRunning, "still running"); err != nil {
		t.Fatal(err)
	}
}

func TestModelAndToolTransitions(t *testing.T) {
	modelCases := []struct {
		from, to ModelStatus
		valid    bool
	}{
		{ModelIdle, ModelRequesting, true},
		{ModelRequesting, ModelStreaming, true},
		{ModelStreaming, ModelFinished, true},
		{ModelFinished, ModelRequesting, false},
	}
	for _, tc := range modelCases {
		if got := validModelTransition(tc.from, tc.to); got != tc.valid {
			t.Errorf("model transition %s -> %s = %v, want %v", tc.from, tc.to, got, tc.valid)
		}
	}

	toolCases := []struct {
		from, to ToolStatus
		valid    bool
	}{
		{ToolPending, ToolRunning, true},
		{ToolRunning, ToolSucceeded, true},
		{ToolRunning, ToolTimedOut, true},
		{ToolSucceeded, ToolRunning, false},
	}
	for _, tc := range toolCases {
		if got := validToolTransition(tc.from, tc.to); got != tc.valid {
			t.Errorf("tool transition %s -> %s = %v, want %v", tc.from, tc.to, got, tc.valid)
		}
	}
}
