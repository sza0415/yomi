package trace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testEvent(sessionID, runID, eventType string, sequence uint64, ts time.Time) Event {
	return Event{Sequence: sequence, Timestamp: ts, SessionID: sessionID, RunID: runID, AgentID: "a", Type: eventType}
}

func TestJSONLSinkReadRunAndSession(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewJSONLSink(dir)
	if err != nil {
		t.Fatal(err)
	}

	events := []Event{
		testEvent("s1", "r1", EventRunStarted, 1, time.Unix(10, 0)),
		testEvent("s1", "r1", EventRunFinished, 2, time.Unix(11, 0)),
		testEvent("s2", "r2", EventRunStarted, 1, time.Unix(12, 0)),
	}
	for _, event := range events {
		if err := sink.Record(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}

	run, err := sink.ReadRun("r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(run) != 2 || run[0].SchemaVersion != SchemaVersion || run[1].Type != EventRunFinished {
		t.Fatalf("ReadRun() = %#v", run)
	}

	session, err := sink.ReadSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(session) != 2 || session[0].RunID != "r1" || session[1].Sequence != 2 {
		t.Fatalf("ReadSession() = %#v", session)
	}

	missing, err := sink.ReadRun("missing")
	if err != nil || len(missing) != 0 {
		t.Fatalf("ReadRun(missing) = %#v, %v", missing, err)
	}

	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("trace directory mode = %v, err = %v", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(sink.path("r1")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trace file mode = %v, err = %v", info.Mode().Perm(), err)
	}
}

func TestJSONLSinkRunIDsDoNotCollide(t *testing.T) {
	sink, err := NewJSONLSink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, event := range []Event{
		testEvent("s", "outside", EventRunStarted, 1, now),
		testEvent("s", "../../outside", EventRunStarted, 1, now.Add(time.Second)),
	} {
		if err := sink.Record(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if sink.path("outside") == sink.path("../../outside") {
		t.Fatal("distinct run IDs mapped to the same file")
	}
	got, err := sink.ReadRun("../../outside")
	if err != nil || len(got) != 1 || got[0].RunID != "../../outside" {
		t.Fatalf("ReadRun() = %#v, %v", got, err)
	}
	if filepath.Dir(sink.path("../../outside")) != sink.dir {
		t.Fatalf("run path escaped trace directory: %q", sink.path("../../outside"))
	}
}

func TestReadSessionSkipsDamagedUnrelatedRun(t *testing.T) {
	sink, err := NewJSONLSink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Record(context.Background(), testEvent("target", "good", EventRunStarted, 1, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sink.path("broken"), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := sink.ReadSession("target")
	if err != nil || len(got) != 1 || got[0].RunID != "good" {
		t.Fatalf("ReadSession() = %#v, %v", got, err)
	}
}
