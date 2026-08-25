package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestReportOptionsControlDailyAndTokenOutput(t *testing.T) {
	entries := []LogEntry{
		{ID: "second", CreatedAt: mustTime(t, "2026-08-25T04:00:00Z"), Provider: "openai", Model: "gpt", Duration: 20, TokensIn: 20, TokensOut: 3, Cost: 0.2, Metadata: map[string]string{"x-session-id": "b"}},
		{ID: "first", CreatedAt: mustTime(t, "2026-08-24T04:00:00Z"), Provider: "anthropic", Model: "claude", Duration: 10, TokensIn: 10, TokensOut: 2, Cost: 0.1, Metadata: map[string]string{"x-session-id": "a"}},
	}
	cfg := config{userID: "user", start: mustTime(t, "2026-08-24T00:00:00Z"), end: mustTime(t, "2026-08-26T00:00:00Z")}
	defaultReport := report(entries, cfg, false)
	for _, want := range []string{"**User:** user", "**Sessions:** 2", "anthropic/claude", "2026-08-24 04:00:00–04:00:00 (10ms)", "| Session | Period | Model | Requests | Cost ($) |"} {
		if !strings.Contains(defaultReport, want) {
			t.Errorf("report does not contain %q", want)
		}
	}
	if strings.Contains(defaultReport, "Tokens In") || strings.Contains(defaultReport, "## Daily usage") {
		t.Error("default report includes optional sections")
	}

	detailedReport := report(entries, config{userID: "user", daily: true, allDaily: true, showTokens: true}, true)
	for _, want := range []string{"Tokens In", "## Daily usage (all models)", "## Daily usage: anthropic/claude", "| **Total** | **1** | **1** | **10** | **2** | **0.100000** |"} {
		if !strings.Contains(detailedReport, want) {
			t.Errorf("detailed report does not contain %q", want)
		}
	}
}

func TestFetchLogsPaginatesAndAuthenticates(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		pages = append(pages, r.URL.Query().Get("page"))
		if r.URL.Query().Get("filters") == "" {
			t.Error("request has no filters")
		}
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"one","created_at":"2026-08-24T00:00:00Z","metadata":{"cf.user_id":"user"}}],"result_info":{"page":1,"per_page":1,"total_count":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"two","created_at":"2026-08-24T01:00:00Z","metadata":{"cf.user_id":"user"}}],"result_info":{"page":2,"per_page":1,"total_count":2}}`))
	}))
	defer server.Close()

	entries, _, err := fetchLogs(server.Client(), config{accountID: "account", gateway: "gateway", userID: "user", apiBase: server.URL, start: mustTime(t, "2026-08-24T00:00:00Z"), end: mustTime(t, "2026-08-25T00:00:00Z")}, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || strings.Join(pages, ",") != "1,2" {
		t.Fatalf("entries=%d pages=%v", len(entries), pages)
	}
}

func TestParseLookbackSupportsDays(t *testing.T) {
	got, err := parseLookback("7d")
	if err != nil {
		t.Fatal(err)
	}
	if want := 7 * 24 * time.Hour; got != want {
		t.Fatalf("parseLookback(7d) = %v, want %v", got, want)
	}
	if _, err := parseLookback("0d"); err == nil {
		t.Fatal("parseLookback accepted zero duration")
	}
}

func TestSessionModelGroupsAggregateRequests(t *testing.T) {
	entries := []LogEntry{
		{ID: "two", CreatedAt: mustTime(t, "2026-08-24T02:00:00Z"), Model: "model", Provider: "provider", Duration: 20, TokensIn: 20, TokensOut: 2, Cost: 0.2, Metadata: map[string]string{"x-session-id": "session"}},
		{ID: "one", CreatedAt: mustTime(t, "2026-08-24T01:00:00Z"), Model: "model", Provider: "provider", Duration: 10, TokensIn: 10, TokensOut: 1, Cost: 0.1, Metadata: map[string]string{"x-session-id": "session"}},
		{ID: "three", CreatedAt: mustTime(t, "2026-08-24T03:00:00Z"), Model: "other", Metadata: map[string]string{"x-session-id": "session"}},
	}
	groups := sessionModelGroups(entries, false)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	group := groups[1]
	if group.Model != "provider/model" || group.Totals.Requests != 2 || group.Totals.Duration != 30 || group.Totals.TokensIn != 30 || group.Totals.TokensOut != 3 || group.Totals.Cost < 0.299999 || group.Totals.Cost > 0.300001 {
		t.Fatalf("unexpected group: %#v", group)
	}
	joined := sessionModelGroups(entries, true)
	if len(joined) != 1 || joined[0].Model != "/other, provider/model" || joined[0].Totals.Requests != 3 || joined[0].Totals.Duration != 30 {
		t.Fatalf("unexpected joined group: %#v", joined)
	}
}

func TestFilterEntriesForUserOnlyFiltersMixedInput(t *testing.T) {
	singleUser := []LogEntry{{ID: "one", Metadata: map[string]string{"cf.user_id": "other"}}}
	if got := filterEntriesForUser(singleUser, "requested"); len(got) != 1 {
		t.Fatalf("single-user input was filtered: %#v", got)
	}
	mixedUsers := append(singleUser, LogEntry{ID: "two", Metadata: map[string]string{"cf.user_id": "requested"}})
	got := filterEntriesForUser(mixedUsers, "requested")
	if len(got) != 1 || got[0].ID != "two" {
		t.Fatalf("mixed input filter = %#v", got)
	}
}

func TestLogsURLBuildsMetadataFilters(t *testing.T) {
	cfg := config{accountID: "account", gateway: "gateway", userID: "user", apiBase: "https://example.test/api/v4", start: mustTime(t, "2026-08-24T00:00:00Z"), end: mustTime(t, "2026-08-25T00:00:00Z")}
	raw, err := logsURL(cfg, 3)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/api/v4/accounts/account/ai-gateway/gateways/gateway/logs" || u.Query().Get("page") != "3" || !strings.Contains(u.Query().Get("filters"), `"cf.user_id"`) {
		t.Fatalf("unexpected URL: %s", raw)
	}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
