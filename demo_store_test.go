package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTrimDemoStatePurgesOlderThanTTL(t *testing.T) {
	now := time.Now()
	st := demoState{
		Keys: []demoKey{
			{ID: "old", CreatedAt: now.Add(-3 * time.Hour).UnixMilli()},
			{ID: "new", CreatedAt: now.Add(-10 * time.Minute).UnixMilli()},
		},
		Events: []demoEvent{
			{Type: "old", CreatedAt: now.Add(-3 * time.Hour).UnixMilli()},
			{Type: "new", CreatedAt: now.Add(-10 * time.Minute).UnixMilli()},
		},
	}

	got := trimDemoState(st, now)

	if len(got.Keys) != 1 || got.Keys[0].ID != "new" {
		t.Fatalf("keys after trim = %#v", got.Keys)
	}
	if len(got.Events) != 1 || got.Events[0].Type != "new" {
		t.Fatalf("events after trim = %#v", got.Events)
	}
}

func TestDemoStoreAddKeyPersistsAndSetsExpiry(t *testing.T) {
	ds := NewDemoStore(filepath.Join(t.TempDir(), "demo_state.json"))

	k, st, err := ds.AddKey("guest", "gpt-5.5", 123, "demo notes", ptrInt64(2))
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	if k.Owner != "guest" || k.Model != "gpt-5.5" || k.DailyBudget != 123 || k.Notes != "demo notes" || k.GroupID == nil || *k.GroupID != 2 {
		t.Fatalf("unexpected key: %#v", k)
	}
	if k.ExpiresAt <= k.CreatedAt {
		t.Fatalf("expires_at should be after created_at: %#v", k)
	}
	if len(st.Keys) == 0 || st.Keys[0].ID != k.ID {
		t.Fatalf("state missing created key: %#v", st.Keys)
	}

	loaded, err := ds.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Keys) == 0 || loaded.Keys[0].ID != k.ID {
		t.Fatalf("persisted state missing created key: %#v", loaded.Keys)
	}
}

func TestDemoStoreUpdateAndDeleteKeyByProxyKey(t *testing.T) {
	ds := NewDemoStore(filepath.Join(t.TempDir(), "demo_state.json"))
	k, _, err := ds.AddKey("guest", "gpt-5.5", 123, "", nil)
	if err != nil {
		t.Fatalf("add key: %v", err)
	}

	owner := "alice"
	budget := 456.0
	notes := "edited"
	groupID := int64(3)
	updated, _, err := ds.UpdateKey(k.Key, demoKeyPatch{
		Owner:       &owner,
		DailyBudget: &budget,
		Notes:       &notes,
		GroupID:     &groupID,
	})
	if err != nil {
		t.Fatalf("update key: %v", err)
	}
	if updated.Owner != owner || updated.DailyBudget != budget || updated.Notes != notes || updated.GroupID == nil || *updated.GroupID != groupID {
		t.Fatalf("unexpected updated key: %#v", updated)
	}

	st, err := ds.DeleteKey(k.Key)
	if err != nil {
		t.Fatalf("delete key: %v", err)
	}
	for _, existing := range st.Keys {
		if existing.Key == k.Key {
			t.Fatalf("deleted key still present: %#v", st.Keys)
		}
	}
}

func TestResetDemoSeedKeepsVisitorDataAndRefreshesSeed(t *testing.T) {
	now := time.Now()
	oldSeed := seedDemoStateAt(now.Add(-3 * time.Hour))
	st := demoState{
		Keys: append([]demoKey{{
			ID:        "visitor",
			Owner:     "guest",
			CreatedAt: now.Add(-10 * time.Minute).UnixMilli(),
			ExpiresAt: now.Add(time.Hour).UnixMilli(),
		}}, oldSeed.Keys...),
		Events: append([]demoEvent{{
			Type:      "visitor",
			Message:   "guest created key",
			CreatedAt: now.Add(-10 * time.Minute).UnixMilli(),
		}}, oldSeed.Events...),
	}

	got := resetDemoSeed(trimDemoState(st, now), now)

	if len(got.Keys) != 5 {
		t.Fatalf("keys = %d, want visitor + 4 seed: %#v", len(got.Keys), got.Keys)
	}
	if got.Keys[0].ID != "visitor" {
		t.Fatalf("visitor key should stay first: %#v", got.Keys)
	}
	seedCount := 0
	for _, k := range got.Keys {
		if k.Seed {
			seedCount++
			if k.ExpiresAt <= now.UnixMilli() {
				t.Fatalf("seed expiry was not refreshed: %#v", k)
			}
		}
	}
	if seedCount != 4 {
		t.Fatalf("seed key count = %d", seedCount)
	}
}
