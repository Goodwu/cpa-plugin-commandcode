package main

import (
	"testing"
	"time"
)

func TestStateRollingWindow(t *testing.T) {
	store := newStateStore()
	now := time.Now()
	store.recordRequest(requestRecord{AuthID: "a", At: now.Add(-6 * time.Hour)})
	store.recordRequest(requestRecord{AuthID: "a", At: now.Add(-2 * time.Hour)})
	store.recordRequest(requestRecord{AuthID: "a", At: now.Add(-time.Minute)})

	snapshot := store.snapshot()
	if len(snapshot.Auths) != 1 {
		t.Fatalf("auths = %d", len(snapshot.Auths))
	}
	if snapshot.Auths[0].Requests5h != 2 {
		t.Fatalf("requests5h = %d, want 2", snapshot.Auths[0].Requests5h)
	}
	if snapshot.Totals.Requests != 3 {
		t.Fatalf("total requests = %d", snapshot.Totals.Requests)
	}
}

func TestStateTotalsAndReset(t *testing.T) {
	store := newStateStore()
	store.recordRequest(requestRecord{AuthID: "a", Status: 429, PromptTokens: 10, CompletionTokens: 5})
	store.recordRequest(requestRecord{AuthID: "a", PromptTokens: 20, CachedTokens: 3})

	snapshot := store.snapshot()
	if snapshot.Totals.Failures != 1 {
		t.Fatalf("failures = %d", snapshot.Totals.Failures)
	}
	if snapshot.Totals.PromptTokens != 30 || snapshot.Totals.CompletionTokens != 5 || snapshot.Totals.CachedTokens != 3 {
		t.Fatalf("totals = %+v", snapshot.Totals)
	}
	if len(snapshot.Recent) != 2 {
		t.Fatalf("recent = %d", len(snapshot.Recent))
	}
	// Newest first
	if snapshot.Recent[0].PromptTokens != 20 {
		t.Fatalf("recent order = %+v", snapshot.Recent)
	}

	store.reset()
	after := store.snapshot()
	if after.Totals.Requests != 0 || len(after.Recent) != 0 {
		t.Fatalf("reset did not clear: %+v", after.Totals)
	}
	if len(after.Auths) != 1 {
		t.Fatal("reset must keep learned auth metadata")
	}
}

func TestStateRecordCap(t *testing.T) {
	store := newStateStore()
	for i := 0; i < requestHistory+20; i++ {
		store.recordRequest(requestRecord{AuthID: "a"})
	}
	if got := len(store.snapshot().Recent); got != 20 {
		t.Fatalf("recent capped at 20 per snapshot, got %d", got)
	}
	if got := len(store.records); got != requestHistory {
		t.Fatalf("records = %d, want %d", got, requestHistory)
	}
}
