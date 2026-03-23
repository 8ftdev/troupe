package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	gh "github.com/8ftdev/troupe/internal/github"
	"github.com/8ftdev/troupe/internal/plane"
	"github.com/8ftdev/troupe/internal/runner"

	"github.com/8ftdev/troupe/internal/config"
)

// --- mock types ---

type mockPlane struct {
	mu       sync.Mutex
	states   map[string]string           // name → ID
	items    map[string][]plane.WorkItem // stateID → items
	comments []commentRecord
	updates  []updateRecord
	// Error injection
	resolveErr error
	listErr    error
	updateErr  error
	commentErr error
}

type commentRecord struct {
	WorkItemID string
	HTML       string
}

type updateRecord struct {
	WorkItemID string
	Req        plane.UpdateWorkItemRequest
}

func (m *mockPlane) ResolveStateID(_ context.Context, name string) (string, error) {
	if m.resolveErr != nil {
		return "", m.resolveErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.states[name]
	if !ok {
		return "", fmt.Errorf("state %q not found", name)
	}
	return id, nil
}

func (m *mockPlane) ListWorkItems(_ context.Context, stateID string) ([]plane.WorkItem, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.items[stateID], nil
}

func (m *mockPlane) UpdateWorkItem(_ context.Context, id string, req *plane.UpdateWorkItemRequest) (*plane.WorkItem, error) {
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, updateRecord{WorkItemID: id, Req: *req})
	return &plane.WorkItem{ID: id, StateID: req.StateID}, nil
}

func (m *mockPlane) CreateComment(_ context.Context, workItemID, html string) (*plane.Comment, error) {
	if m.commentErr != nil {
		return nil, m.commentErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments = append(m.comments, commentRecord{WorkItemID: workItemID, HTML: html})
	return &plane.Comment{ID: "comment-1"}, nil
}

func (m *mockPlane) getComments() []commentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]commentRecord, len(m.comments))
	copy(out, m.comments)
	return out
}

func (m *mockPlane) getUpdates() []updateRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]updateRecord, len(m.updates))
	copy(out, m.updates)
	return out
}

type mockGitHub struct {
	issues []gh.Issue
	err    error
}

func (m *mockGitHub) FetchIssues(_ context.Context, _ []string) ([]gh.Issue, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.issues, nil
}

type mockRunner struct {
	result     *runner.Result
	err        error
	promptSeen string
}

func (m *mockRunner) Run(_ context.Context, prompt string) (*runner.Result, error) {
	m.promptSeen = prompt
	return m.result, m.err
}

// --- helpers ---

func testConfig() *config.Config {
	return &config.Config{
		Name: "test-project",
		Repo: "owner/repo",
		Agent: config.AgentConfig{
			Model:    "claude-sonnet-4-6",
			MaxTurns: 50,
		},
		GitHub: config.GitHubConfig{
			Labels: []string{"agent-ready"},
		},
		Plane: config.PlaneConfig{
			TriggerState: "In Progress",
			DoneState:    "Done",
			FailedState:  "Failed",
		},
		PromptTemplate: "Fix issue #{{.Issue.Number}}: {{.Issue.Title}}\n\n{{.Issue.Body}}",
	}
}

func testPlane() *mockPlane {
	return &mockPlane{
		states: map[string]string{
			"In Progress": "state-ip",
			"Done":        "state-done",
			"Failed":      "state-failed",
		},
		items: make(map[string][]plane.WorkItem),
	}
}

func testGitHub() *mockGitHub {
	return &mockGitHub{
		issues: []gh.Issue{
			{Number: 42, Title: "Fix typo", Body: "There's a typo in README", State: "open", CreatedAt: time.Now()},
			{Number: 43, Title: "Add feature", Body: "Add new feature", State: "open", CreatedAt: time.Now()},
		},
	}
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- tests ---

func TestProcessNext_NoTriggeredItems(t *testing.T) {
	p := testPlane()
	g := testGitHub()

	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), nil)

	processed, err := o.processNext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processed {
		t.Fatal("expected no items processed")
	}
}

func TestProcessNext_SuccessWithPR(t *testing.T) {
	p := testPlane()
	p.items["state-ip"] = []plane.WorkItem{
		{ID: "card-1", Name: "Fix typo", ExternalSource: "github", ExternalID: "42", CreatedAt: time.Now()},
	}

	g := testGitHub()
	mr := &mockRunner{result: &runner.Result{
		Success:   true,
		PRCreated: true,
		PRURL:     "https://github.com/owner/repo/pull/1",
		NumTurns:  5,
		Duration:  30 * time.Second,
		CostUSD:   0.50,
	}}

	factory := func(_ runner.ProgressFunc) AgentRunner { return mr }
	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), factory)

	processed, err := o.processNext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !processed {
		t.Fatal("expected item to be processed")
	}

	// Verify prompt was rendered correctly.
	if mr.promptSeen == "" {
		t.Fatal("runner was not called")
	}
	if got := mr.promptSeen; got != "Fix issue #42: Fix typo\n\nThere's a typo in README" {
		t.Errorf("unexpected prompt: %q", got)
	}

	// Verify card moved to Done.
	updates := p.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Req.StateID != "state-done" {
		t.Errorf("expected state-done, got %q", updates[0].Req.StateID)
	}

	// Verify comments: "starting" + "PR created".
	comments := p.getComments()
	if len(comments) < 2 {
		t.Fatalf("expected at least 2 comments, got %d", len(comments))
	}
	if comments[0].HTML != "<p>Agent starting...</p>" {
		t.Errorf("unexpected first comment: %q", comments[0].HTML)
	}
}

func TestProcessNext_AgentFailure(t *testing.T) {
	p := testPlane()
	p.items["state-ip"] = []plane.WorkItem{
		{ID: "card-1", Name: "Fix typo", ExternalSource: "github", ExternalID: "42", CreatedAt: time.Now()},
	}

	g := testGitHub()
	mr := &mockRunner{result: &runner.Result{
		Success: false,
		Error:   "exceeded maximum turns",
	}}

	factory := func(_ runner.ProgressFunc) AgentRunner { return mr }
	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), factory)

	processed, err := o.processNext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !processed {
		t.Fatal("expected item to be processed")
	}

	// Verify card moved to Failed.
	updates := p.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Req.StateID != "state-failed" {
		t.Errorf("expected state-failed, got %q", updates[0].Req.StateID)
	}
}

func TestProcessNext_SuccessNoPR(t *testing.T) {
	p := testPlane()
	p.items["state-ip"] = []plane.WorkItem{
		{ID: "card-1", Name: "Fix typo", ExternalSource: "github", ExternalID: "42", CreatedAt: time.Now()},
	}

	g := testGitHub()
	mr := &mockRunner{result: &runner.Result{Success: true, PRCreated: false}}

	factory := func(_ runner.ProgressFunc) AgentRunner { return mr }
	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), factory)

	_, err := o.processNext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should fail card since no PR was created.
	updates := p.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Req.StateID != "state-failed" {
		t.Errorf("expected state-failed, got %q", updates[0].Req.StateID)
	}
}

func TestProcessNext_RunnerError(t *testing.T) {
	p := testPlane()
	p.items["state-ip"] = []plane.WorkItem{
		{ID: "card-1", Name: "Fix typo", ExternalSource: "github", ExternalID: "42", CreatedAt: time.Now()},
	}

	g := testGitHub()
	mr := &mockRunner{err: fmt.Errorf("before_run hook failed")}

	factory := func(_ runner.ProgressFunc) AgentRunner { return mr }
	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), factory)

	_, err := o.processNext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should fail card with error.
	updates := p.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Req.StateID != "state-failed" {
		t.Errorf("expected state-failed, got %q", updates[0].Req.StateID)
	}

	comments := p.getComments()
	found := false
	for _, c := range comments {
		if c.HTML == "<p>Agent failed: Agent error: before_run hook failed</p>" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected error comment, got: %v", comments)
	}
}

func TestProcessNext_NoGitHubLink(t *testing.T) {
	p := testPlane()
	p.items["state-ip"] = []plane.WorkItem{
		{ID: "card-1", Name: "Manual card", ExternalSource: "", ExternalID: "", CreatedAt: time.Now()},
	}

	g := testGitHub()
	factory := func(_ runner.ProgressFunc) AgentRunner { return &mockRunner{} }
	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), factory)

	_, err := o.processNext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should fail card because no GitHub link.
	updates := p.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Req.StateID != "state-failed" {
		t.Errorf("expected state-failed, got %q", updates[0].Req.StateID)
	}
}

func TestProcessNext_GitHubIssueMissing(t *testing.T) {
	p := testPlane()
	p.items["state-ip"] = []plane.WorkItem{
		{ID: "card-1", Name: "Deleted issue", ExternalSource: "github", ExternalID: "999", CreatedAt: time.Now()},
	}

	g := testGitHub()
	factory := func(_ runner.ProgressFunc) AgentRunner { return &mockRunner{} }
	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), factory)

	_, err := o.processNext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updates := p.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Req.StateID != "state-failed" {
		t.Errorf("expected state-failed, got %q", updates[0].Req.StateID)
	}
}

func TestProcessNext_ResolveStateError(t *testing.T) {
	p := testPlane()
	p.resolveErr = fmt.Errorf("network error")

	o := New(testConfig(), p, testGitHub(), "/tmp", 5*time.Second, silentLogger(), nil)

	_, err := o.processNext(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestProcessNext_ListWorkItemsError(t *testing.T) {
	p := testPlane()
	p.listErr = fmt.Errorf("network error")

	o := New(testConfig(), p, testGitHub(), "/tmp", 5*time.Second, silentLogger(), nil)

	_, err := o.processNext(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestProcessNext_PicksOldestCard(t *testing.T) {
	now := time.Now()
	p := testPlane()
	p.items["state-ip"] = []plane.WorkItem{
		{ID: "card-new", Name: "Newer", ExternalSource: "github", ExternalID: "43", CreatedAt: now},
		{ID: "card-old", Name: "Older", ExternalSource: "github", ExternalID: "42", CreatedAt: now.Add(-1 * time.Hour)},
	}

	g := testGitHub()
	var seenCardID string
	mp := &mockPlane{
		states: p.states,
		items:  p.items,
	}

	mr := &mockRunner{result: &runner.Result{Success: true, PRCreated: true, PRURL: "https://github.com/owner/repo/pull/1"}}
	factory := func(_ runner.ProgressFunc) AgentRunner { return mr }

	o := New(testConfig(), mp, g, "/tmp", 5*time.Second, silentLogger(), factory)
	_, _ = o.processNext(context.Background())

	updates := mp.getUpdates()
	if len(updates) > 0 {
		seenCardID = updates[0].WorkItemID
	}
	if seenCardID != "card-old" {
		t.Errorf("expected oldest card (card-old), got %q", seenCardID)
	}
}

func TestProcessNext_ProgressCallbackPostsComments(t *testing.T) {
	p := testPlane()
	p.items["state-ip"] = []plane.WorkItem{
		{ID: "card-1", Name: "Fix typo", ExternalSource: "github", ExternalID: "42", CreatedAt: time.Now()},
	}

	g := testGitHub()
	var capturedProgress runner.ProgressFunc
	mr := &mockRunner{result: &runner.Result{Success: true, PRCreated: true, PRURL: "https://github.com/owner/repo/pull/1"}}

	factory := func(onProgress runner.ProgressFunc) AgentRunner {
		capturedProgress = onProgress
		return mr
	}

	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), factory)
	_, _ = o.processNext(context.Background())

	// Simulate progress callback.
	if capturedProgress != nil {
		capturedProgress("Working on task 1...")
	}

	comments := p.getComments()
	found := false
	for _, c := range comments {
		if c.HTML == "<p>Working on task 1...</p>" && c.WorkItemID == "card-1" {
			found = true
		}
	}
	if !found {
		t.Error("expected progress comment scoped to card-1")
	}
}

func TestPickOldest_Empty(t *testing.T) {
	if got := pickOldest(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestPickOldest_SingleItem(t *testing.T) {
	items := []plane.WorkItem{{ID: "a", CreatedAt: time.Now()}}
	got := pickOldest(items)
	if got == nil || got.ID != "a" {
		t.Errorf("expected item a, got %v", got)
	}
}

func TestPickOldest_MultipleItems(t *testing.T) {
	now := time.Now()
	items := []plane.WorkItem{
		{ID: "b", CreatedAt: now},
		{ID: "a", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "c", CreatedAt: now.Add(-1 * time.Hour)},
	}
	got := pickOldest(items)
	if got == nil || got.ID != "a" {
		t.Errorf("expected oldest item a, got %v", got)
	}
}

func TestRun_ShutdownOnCancel(t *testing.T) {
	p := testPlane()
	g := testGitHub()

	o := New(testConfig(), p, g, "/tmp", 50*time.Millisecond, silentLogger(), nil)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- o.Run(ctx)
	}()

	// Let it poll once, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("orchestrator did not shut down in time")
	}
}

func TestRun_ProcessesItemThenContinues(t *testing.T) {
	p := testPlane()
	p.items["state-ip"] = []plane.WorkItem{
		{ID: "card-1", Name: "Fix typo", ExternalSource: "github", ExternalID: "42", CreatedAt: time.Now()},
	}

	g := testGitHub()
	callCount := 0
	factory := func(_ runner.ProgressFunc) AgentRunner {
		callCount++
		return &mockRunner{result: &runner.Result{Success: true, PRCreated: true, PRURL: "https://github.com/owner/repo/pull/1"}}
	}

	o := New(testConfig(), p, g, "/tmp", 50*time.Millisecond, silentLogger(), factory)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- o.Run(ctx)
	}()

	// Let it process and poll again.
	time.Sleep(200 * time.Millisecond)
	cancel()

	<-done

	if callCount < 1 {
		t.Error("expected at least 1 runner invocation")
	}
}

func TestMatchGitHubIssue_InvalidExternalID(t *testing.T) {
	p := testPlane()
	g := testGitHub()
	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), nil)

	card := &plane.WorkItem{
		ExternalSource: "github",
		ExternalID:     "not-a-number",
	}
	_, err := o.matchGitHubIssue(context.Background(), card)
	if err == nil {
		t.Fatal("expected error for non-numeric external_id")
	}
}

func TestMatchGitHubIssue_FetchError(t *testing.T) {
	p := testPlane()
	g := &mockGitHub{err: fmt.Errorf("API error")}
	o := New(testConfig(), p, g, "/tmp", 5*time.Second, silentLogger(), nil)

	card := &plane.WorkItem{
		ExternalSource: "github",
		ExternalID:     "42",
	}
	_, err := o.matchGitHubIssue(context.Background(), card)
	if err == nil {
		t.Fatal("expected error on fetch failure")
	}
}
