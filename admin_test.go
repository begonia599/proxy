package main

import (
	"testing"
	"time"
)

func TestParseUntilDateIncludesWholeDay(t *testing.T) {
	got, err := parseUntil("2026-06-21")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 22, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("parseUntil() = %v, want %v", got, want)
	}
}

func TestParseUntilTimestampRemainsExact(t *testing.T) {
	got, err := parseUntil("2026-06-21 16:30:00")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 21, 16, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("parseUntil() = %v, want %v", got, want)
	}
}

func TestCreatorScopedStatsAndLogs(t *testing.T) {
	s := newTempStore(t)
	for _, k := range []KeyMeta{
		{Key: "sk-alice", Owner: "alice", Creator: "alice", AllowedModels: "*"},
		{Key: "sk-bob", Owner: "bob", Creator: "bob", AllowedModels: "*"},
	} {
		if err := s.CreateKey(&k); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli()
	for _, row := range []struct {
		key  string
		cost float64
	}{{"sk-alice", 1.25}, {"sk-bob", 9.75}} {
		if _, err := s.db.Exec(
			"INSERT INTO requests(ts, proxy_key, endpoint, method, status, model, cost_usd) VALUES (?, ?, '/v1/messages', 'POST', 200, 'm', ?)",
			now, row.key, row.cost); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := s.Stats(StatsFilter{Creator: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 1 || stats.CostUSD != 1.25 {
		t.Fatalf("alice stats = requests %d cost %v", stats.Requests, stats.CostUSD)
	}
	logs, err := s.ListLogs(LogFilter{Creator: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].ProxyKey != "sk-bob" {
		t.Fatalf("bob logs = %#v", logs)
	}
}

func TestPopulateTodayUsage(t *testing.T) {
	s := newTempStore(t)
	k := KeyMeta{Key: "sk-budget", Owner: "o", Creator: "admin", AllowedModels: "*", DailyBudget: 10}
	if err := s.CreateKey(&k); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		"INSERT INTO requests(ts, proxy_key, status, cost_usd) VALUES (?, ?, 200, ?), (?, ?, 200, ?)",
		time.Now().UnixMilli(), k.Key, 2.25, time.Now().UnixMilli(), k.Key, 1.5); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListKeys(false, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PopulateTodayUsage(list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].TodayRequests != 2 || list[0].TodayCost != 3.75 {
		t.Fatalf("today usage = %#v", list)
	}
	if list[0].BudgetRemain == nil || *list[0].BudgetRemain != 6.25 {
		t.Fatalf("remaining budget = %v", list[0].BudgetRemain)
	}
}
