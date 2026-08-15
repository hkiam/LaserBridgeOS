package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The record is written to /data so that it outlives a reboot. It used to
// outlive it only on disk: the web interface reads the daemon's ring, and the
// ring started empty, so an incident became invisible in exactly the moment it
// mattered - a stationary beam is stopped, the operator changes a setting, the
// bridge restarts, and the account of what just happened is gone from the page
// they were reading.
func TestTheRecordSurvivesTheRestartItCaused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal", "events.jsonl")

	before := NewJournal(path)
	before.Add(Event{Kind: "intervention", Text: "soft reset after a stationary beam"})
	before.Add(Event{Kind: "alarm", Text: "ALARM:1", Code: 1})
	before.Close()

	after := NewJournal(path)
	defer after.Close()
	events := after.Recent(0)
	if len(events) != 2 {
		t.Fatalf("recovered %d events, want the two that were written", len(events))
	}
	if events[0].Text != "soft reset after a stationary beam" || events[1].Code != 1 {
		t.Fatalf("recovered in the wrong order or shape: %+v", events)
	}
	// And the new run keeps writing to the same file rather than starting over.
	after.Add(Event{Kind: "intervention", Text: "dropped 10.0.0.5:1234 because the bridge took the machine over"})
	after.Close()
	third := NewJournal(path)
	defer third.Close()
	if events := third.Recent(0); len(events) != 3 {
		t.Fatalf("the file was truncated by the run that read it: %d events", len(events))
	}
}

func TestAHalfWrittenLineDoesNotCostTheRest(t *testing.T) {
	// What a power cut leaves behind. The lines before it are still an account
	// of what happened and must still be readable.
	path := filepath.Join(t.TempDir(), "events.jsonl")
	good := `{"unix":1786579200,"kind":"alarm","text":"ALARM:1","uptime_seconds":12}`
	if err := os.WriteFile(path, []byte(good+"\n{\"unix\":178657920"), 0644); err != nil {
		t.Fatal(err)
	}
	journal := NewJournal(path)
	defer journal.Close()
	events := journal.Recent(0)
	if len(events) != 1 || events[0].Text != "ALARM:1" {
		t.Fatalf("events = %+v, want the one complete line", events)
	}
}

func TestTheRotatedGenerationIsReadFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	older := `{"unix":1,"kind":"alarm","text":"older"}`
	newer := `{"unix":2,"kind":"alarm","text":"newer"}`
	if err := os.WriteFile(path+".1", []byte(older+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(newer+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	journal := NewJournal(path)
	defer journal.Close()
	events := journal.Recent(0)
	if len(events) != 2 || events[0].Text != "older" || events[1].Text != "newer" {
		t.Fatalf("events = %+v, want the rotated generation first", events)
	}
	_ = strings.TrimSpace("")
}
