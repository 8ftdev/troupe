package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchIssues(t *testing.T) {
	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	issues := []apiIssue{
		{Number: 2, Title: "Second", State: "open", CreatedAt: now.Add(time.Hour), Labels: []apiLabel{{Name: "agent-ready"}}},
		{Number: 1, Title: "First", State: "open", CreatedAt: now, Labels: []apiLabel{{Name: "agent-ready"}}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer srv.Close()

	c := &Client{
		owner:   "owner",
		repo:    "repo",
		baseURL: srv.URL,
		http:    srv.Client(),
	}

	result, err := c.FetchIssues(context.Background(), []string{"agent-ready"})
	if err != nil {
		t.Fatalf("FetchIssues() error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("got %d issues, want 2", len(result))
	}
	// Should be sorted by created_at ASC.
	if result[0].Number != 1 {
		t.Errorf("first issue number = %d, want 1", result[0].Number)
	}
	if result[1].Number != 2 {
		t.Errorf("second issue number = %d, want 2", result[1].Number)
	}
}

func TestFetchIssues_SkipsPullRequests(t *testing.T) {
	pr := struct{}{}
	issues := []apiIssue{
		{Number: 1, Title: "Issue", State: "open"},
		{Number: 2, Title: "PR", State: "open", PullRequest: &pr},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer srv.Close()

	c := &Client{owner: "o", repo: "r", baseURL: srv.URL, http: srv.Client()}
	result, err := c.FetchIssues(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchIssues() error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d issues, want 1 (PR should be filtered)", len(result))
	}
	if result[0].Number != 1 {
		t.Errorf("issue number = %d, want 1", result[0].Number)
	}
}

func TestFetchIssues_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	c := &Client{owner: "o", repo: "r", baseURL: srv.URL, http: srv.Client()}
	_, err := c.FetchIssues(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for 403 response, got nil")
	}
}

func TestNewClient(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"owner/repo", false},
		{"invalid", true},
		{"/repo", true},
		{"owner/", true},
		{"", true},
	}
	for _, tt := range tests {
		_, err := NewClient("token", tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("NewClient(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
		}
	}
}
