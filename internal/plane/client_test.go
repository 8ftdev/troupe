package plane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestClient returns a Client pointing at the given test server.
func newTestClient(srv *httptest.Server) *Client {
	return &Client{
		apiKey:    "test-key",
		baseURL:   srv.URL,
		workspace: "ws",
		projectID: "proj-1",
		http:      srv.Client(),
	}
}

func TestListStates(t *testing.T) {
	states := []State{
		{ID: "s1", Name: "Todo", Group: "unstarted", Color: "#ccc"},
		{ID: "s2", Name: "In Progress", Group: "started", Color: "#0f0"},
		{ID: "s3", Name: "Done", Group: "completed", Color: "#00f"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("missing or wrong API key header")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(paginatedResponse[State]{Results: states})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListStates(context.Background())
	if err != nil {
		t.Fatalf("ListStates() error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d states, want 3", len(got))
	}
	if got[1].Name != "In Progress" {
		t.Errorf("state[1].Name = %q, want %q", got[1].Name, "In Progress")
	}
}

func TestListStates_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListStates(context.Background())
	if err == nil {
		t.Fatal("expected error for 403 response, got nil")
	}
}

func TestResolveStateID(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(paginatedResponse[State]{
			Results: []State{
				{ID: "s1", Name: "Todo"},
				{ID: "s2", Name: "In Progress"},
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	ctx := context.Background()

	id, err := c.ResolveStateID(ctx, "In Progress")
	if err != nil {
		t.Fatalf("ResolveStateID() error: %v", err)
	}
	if id != "s2" {
		t.Errorf("got %q, want %q", id, "s2")
	}

	// Second call should use cache (no additional HTTP request).
	id2, err := c.ResolveStateID(ctx, "Todo")
	if err != nil {
		t.Fatalf("ResolveStateID() error: %v", err)
	}
	if id2 != "s1" {
		t.Errorf("got %q, want %q", id2, "s1")
	}
	if calls != 1 {
		t.Errorf("API called %d times, want 1 (should cache)", calls)
	}
}

func TestResolveStateID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(paginatedResponse[State]{
			Results: []State{{ID: "s1", Name: "Todo"}},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ResolveStateID(context.Background(), "Nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown state, got nil")
	}
}

func TestListWorkItems(t *testing.T) {
	items := []WorkItem{
		{ID: "w1", Name: "Fix bug", StateID: "s1", ExternalSource: "troupe", ExternalID: "42"},
		{ID: "w2", Name: "Add feature", StateID: "s2"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(paginatedResponse[WorkItem]{Results: items})

		// Verify state filter is passed as query param when present.
		if state := r.URL.Query().Get("state"); state != "" {
			// Just verify the param is forwarded; filtering is server-side.
			t.Logf("state filter: %s", state)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListWorkItems(context.Background(), "")
	if err != nil {
		t.Fatalf("ListWorkItems() error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
}

func TestListWorkItems_FilterByState(t *testing.T) {
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotState = r.URL.Query().Get("state")
		_ = json.NewEncoder(w).Encode(paginatedResponse[WorkItem]{Results: nil})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.ListWorkItems(context.Background(), "s2")
	if err != nil {
		t.Fatalf("ListWorkItems() error: %v", err)
	}
	if gotState != "s2" {
		t.Errorf("state query param = %q, want %q", gotState, "s2")
	}
}

func TestCreateWorkItem(t *testing.T) {
	now := time.Date(2026, 3, 21, 12, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}

		var req CreateWorkItemRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if req.Name != "Fix login bug" {
			t.Errorf("name = %q, want %q", req.Name, "Fix login bug")
		}
		if req.ExternalSource != "troupe" {
			t.Errorf("external_source = %q, want %q", req.ExternalSource, "troupe")
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(WorkItem{
			ID:             "w-new",
			Name:           req.Name,
			StateID:        req.StateID,
			ExternalSource: req.ExternalSource,
			ExternalID:     req.ExternalID,
			CreatedAt:      now,
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	item, err := c.CreateWorkItem(context.Background(), &CreateWorkItemRequest{
		Name:           "Fix login bug",
		StateID:        "s1",
		ExternalSource: "troupe",
		ExternalID:     "7",
	})
	if err != nil {
		t.Fatalf("CreateWorkItem() error: %v", err)
	}
	if item.ID != "w-new" {
		t.Errorf("ID = %q, want %q", item.ID, "w-new")
	}
}

func TestCreateWorkItem_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"name is required"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.CreateWorkItem(context.Background(), &CreateWorkItemRequest{})
	if err == nil {
		t.Fatal("expected error for 400 response, got nil")
	}
}

func TestUpdateWorkItem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		// Verify work item ID is in the URL path.
		wantSuffix := "/work-items/w1/"
		if got := r.URL.Path; len(got) < len(wantSuffix) || got[len(got)-len(wantSuffix):] != wantSuffix {
			t.Errorf("path = %q, want suffix %q", got, wantSuffix)
		}

		var req UpdateWorkItemRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		_ = json.NewEncoder(w).Encode(WorkItem{
			ID:      "w1",
			Name:    "Fix login bug",
			StateID: req.StateID,
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	item, err := c.UpdateWorkItem(context.Background(), "w1", &UpdateWorkItemRequest{
		StateID: "s3",
	})
	if err != nil {
		t.Fatalf("UpdateWorkItem() error: %v", err)
	}
	if item.StateID != "s3" {
		t.Errorf("StateID = %q, want %q", item.StateID, "s3")
	}
}

func TestCreateComment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}

		var payload struct {
			CommentHTML string `json:"comment_html"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)

		if payload.CommentHTML != "<p>Agent starting...</p>" {
			t.Errorf("comment_html = %q, want %q", payload.CommentHTML, "<p>Agent starting...</p>")
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Comment{
			ID:          "c1",
			CommentHTML: payload.CommentHTML,
			CreatedAt:   time.Now(),
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	comment, err := c.CreateComment(context.Background(), "w1", "<p>Agent starting...</p>")
	if err != nil {
		t.Fatalf("CreateComment() error: %v", err)
	}
	if comment.ID != "c1" {
		t.Errorf("ID = %q, want %q", comment.ID, "c1")
	}
}

func TestFindWorkItemByExternalID(t *testing.T) {
	items := []WorkItem{
		{ID: "w1", Name: "Unlinked", ExternalSource: "", ExternalID: ""},
		{ID: "w2", Name: "Linked", ExternalSource: "troupe", ExternalID: "42"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(paginatedResponse[WorkItem]{Results: items})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	ctx := context.Background()

	// Found.
	item, err := c.FindWorkItemByExternalID(ctx, "troupe", "42")
	if err != nil {
		t.Fatalf("FindWorkItemByExternalID() error: %v", err)
	}
	if item == nil {
		t.Fatal("expected non-nil work item")
	}
	if item.ID != "w2" {
		t.Errorf("ID = %q, want %q", item.ID, "w2")
	}

	// Not found.
	item, err = c.FindWorkItemByExternalID(ctx, "troupe", "999")
	if err != nil {
		t.Fatalf("FindWorkItemByExternalID() error: %v", err)
	}
	if item != nil {
		t.Errorf("expected nil, got %+v", item)
	}
}
