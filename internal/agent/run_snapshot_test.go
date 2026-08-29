package agent

import (
	"testing"
	"time"
)

func TestJSONRunSnapshotStoreSaveLoad(t *testing.T) {
	store, err := NewJSONRunSnapshotStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := NewRun("s1", RunBudget{MaxModelCalls: 3})
	if err := run.Transition(RunRunning, "started"); err != nil {
		t.Fatal(err)
	}
	snapshot := run.Snapshot()
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != run.ID || got.SessionID != "s1" || got.Status != RunRunning || got.Budget.MaxModelCalls != 3 {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestJSONRunSnapshotStoreMarkInterrupted(t *testing.T) {
	store, err := NewJSONRunSnapshotStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	running := NewRun("running", RunBudget{})
	if err := running.Transition(RunRunning, "started"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(running.Snapshot()); err != nil {
		t.Fatal(err)
	}
	completed := NewRun("completed", RunBudget{})
	if err := completed.Transition(RunRunning, "started"); err != nil {
		t.Fatal(err)
	}
	if err := completed.Transition(RunCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(completed.Snapshot()); err != nil {
		t.Fatal(err)
	}

	interrupted, err := store.MarkInterrupted()
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 1 || interrupted[0].ID != running.ID {
		t.Fatalf("interrupted = %#v", interrupted)
	}
	got, err := store.Load(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != RunFailed || got.StatusReason != "interrupted by process restart" || got.Error == "" || got.FinishedAt.IsZero() {
		t.Fatalf("interrupted snapshot = %#v", got)
	}
	final, err := store.Load(completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != RunCompleted || final.StatusReason != "done" {
		t.Fatalf("completed snapshot changed = %#v", final)
	}
	if time.Since(got.UpdatedAt) < 0 {
		t.Fatal("snapshot updated time is in the future")
	}
}
