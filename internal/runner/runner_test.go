package runner

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	claude "github.com/partio-io/claude-agent-sdk-go"

	"github.com/8ftdev/troupe/internal/config"
)

// --- Mock session ---

type mockSession struct {
	messages []claude.Message
	sendErr  error
}

func (m *mockSession) Send(_ context.Context, _ string) error {
	return m.sendErr
}

func (m *mockSession) Stream(_ context.Context) iter.Seq2[claude.Message, error] {
	return func(yield func(claude.Message, error) bool) {
		for _, msg := range m.messages {
			if !yield(msg, nil) {
				return
			}
		}
	}
}

func (m *mockSession) Close() error { return nil }

// --- Pure helper tests ---

func TestParseTaskList(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "task prefix format",
			text: "task 1: write tests\ntask 2: implement code\ntask 3: create PR",
			want: []string{"write tests", "implement code", "create PR"},
		},
		{
			name: "numbered dot format",
			text: "1. Write tests\n2. Implement code\n3. Create PR",
			want: []string{"Write tests", "Implement code", "Create PR"},
		},
		{
			name: "numbered paren format",
			text: "1) Write tests\n2) Implement code",
			want: []string{"Write tests", "Implement code"},
		},
		{
			name: "no tasks",
			text: "This is just regular text without task numbering.",
			want: nil,
		},
		{
			name: "mixed with non-task lines",
			text: "Here's my plan:\n1. Write tests\nSome explanation\n2. Fix bug",
			want: []string{"Write tests", "Fix bug"},
		},
		{
			name: "empty string",
			text: "",
			want: nil,
		},
		{
			name: "double digit tasks",
			text: "10. Refactor module\n11. Add logging",
			want: []string{"Refactor module", "Add logging"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTaskList(tt.text)
			if len(got) != len(tt.want) {
				t.Fatalf("parseTaskList() returned %d tasks, want %d\ngot: %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("task[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestDetectPRCreation(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		input    map[string]any
		want     bool
	}{
		{
			name:     "gh pr create command",
			toolName: "Bash",
			input:    map[string]any{"command": "gh pr create --title 'fix typo' --body 'fixes #42'"},
			want:     true,
		},
		{
			name:     "not bash tool",
			toolName: "Edit",
			input:    map[string]any{"command": "gh pr create"},
			want:     false,
		},
		{
			name:     "different bash command",
			toolName: "Bash",
			input:    map[string]any{"command": "go test ./..."},
			want:     false,
		},
		{
			name:     "no command key",
			toolName: "Bash",
			input:    map[string]any{"file": "test.go"},
			want:     false,
		},
		{
			name:     "gh pr create in pipeline",
			toolName: "Bash",
			input:    map[string]any{"command": "git push && gh pr create --fill"},
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectPRCreation(tt.toolName, tt.input)
			if got != tt.want {
				t.Errorf("detectPRCreation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractPRURL(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "simple pr url",
			text: "https://github.com/owner/repo/pull/123",
			want: "https://github.com/owner/repo/pull/123",
		},
		{
			name: "pr url in output",
			text: "Creating pull request for fix-typo into main\nhttps://github.com/owner/repo/pull/42\n",
			want: "https://github.com/owner/repo/pull/42",
		},
		{
			name: "no pr url",
			text: "Command completed successfully",
			want: "",
		},
		{
			name: "empty text",
			text: "",
			want: "",
		},
		{
			name: "github url but not pr",
			text: "https://github.com/owner/repo/issues/5",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPRURL(tt.text)
			if got != tt.want {
				t.Errorf("extractPRURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatTaskList(t *testing.T) {
	tasks := []string{"Write tests", "Fix bug", "Create PR"}
	got := formatTaskList(tasks)
	want := "<p><strong>Task List</strong></p><ol><li>Write tests</li><li>Fix bug</li><li>Create PR</li></ol>"
	if got != want {
		t.Errorf("formatTaskList() = %q, want %q", got, want)
	}
}

func TestToolResultContent(t *testing.T) {
	tests := []struct {
		name    string
		content any
		want    string
	}{
		{
			name:    "string content",
			content: "https://github.com/owner/repo/pull/1",
			want:    "https://github.com/owner/repo/pull/1",
		},
		{
			name: "array content",
			content: []any{
				map[string]any{"type": "text", "text": "PR created at"},
				map[string]any{"type": "text", "text": "https://github.com/owner/repo/pull/5"},
			},
			want: "PR created at\nhttps://github.com/owner/repo/pull/5",
		},
		{
			name:    "nil content",
			content: nil,
			want:    "<nil>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := &claude.ToolResultBlock{Content: tt.content}
			got := toolResultContent(tb)
			if got != tt.want {
				t.Errorf("toolResultContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunHook(t *testing.T) {
	ctx := context.Background()

	t.Run("successful command", func(t *testing.T) {
		if err := runHook(ctx, ".", "true"); err != nil {
			t.Errorf("runHook() unexpected error: %v", err)
		}
	})

	t.Run("failing command", func(t *testing.T) {
		if err := runHook(ctx, ".", "false"); err == nil {
			t.Error("runHook() expected error for failing command")
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := runHook(ctx, ".", "sleep 10"); err == nil {
			t.Error("runHook() expected error for cancelled context")
		}
	})
}

// --- Runner initialization test ---

func TestNew(t *testing.T) {
	cfg := &config.Config{
		Agent: config.AgentConfig{
			Model:        "claude-sonnet-4-6",
			MaxTurns:     50,
			MaxBudgetUSD: 5.0,
			AllowedTools: []string{"Bash", "Read", "Edit"},
		},
		Hooks: config.HooksConfig{
			BeforeRun: "./setup.sh",
			AfterRun:  "./cleanup.sh",
		},
	}

	r := New(cfg, "/repo", nil)

	if r.model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want %q", r.model, "claude-sonnet-4-6")
	}
	if r.maxTurns != 50 {
		t.Errorf("maxTurns = %d, want %d", r.maxTurns, 50)
	}
	if r.maxBudgetUSD != 5.0 {
		t.Errorf("maxBudgetUSD = %f, want %f", r.maxBudgetUSD, 5.0)
	}
	if len(r.allowedTools) != 3 {
		t.Errorf("allowedTools len = %d, want 3", len(r.allowedTools))
	}
	if r.beforeRun != "./setup.sh" {
		t.Errorf("beforeRun = %q, want %q", r.beforeRun, "./setup.sh")
	}
	if r.afterRun != "./cleanup.sh" {
		t.Errorf("afterRun = %q, want %q", r.afterRun, "./cleanup.sh")
	}
	if r.onProgress == nil {
		t.Error("onProgress should default to noop, not nil")
	}
}

// --- Run method tests with mock session ---

func TestRunSuccess(t *testing.T) {
	cost := 0.50
	mock := &mockSession{
		messages: []claude.Message{
			&claude.SystemMessage{Type: "system", SessionID: "s1"},
			&claude.AssistantMessage{
				Type: "assistant",
				Message: &claude.APIMessage{
					Content: []claude.ContentBlock{
						&claude.TextBlock{Type: "text", Text: "1. Write tests\n2. Fix bug\n3. Create PR"},
					},
				},
			},
			&claude.AssistantMessage{
				Type: "assistant",
				Message: &claude.APIMessage{
					Content: []claude.ContentBlock{
						&claude.ToolUseBlock{
							Type:  "tool_use",
							ID:    "tool-1",
							Name:  "Bash",
							Input: map[string]any{"command": "gh pr create --title 'fix' --body 'body'"},
						},
					},
				},
			},
			&claude.UserMessage{
				Type: "user",
				Message: &claude.UserAPIMessage{
					Content: []claude.ContentBlock{
						&claude.ToolResultBlock{
							Type:      "tool_result",
							ToolUseID: "tool-1",
							Content:   "https://github.com/owner/repo/pull/42",
						},
					},
				},
			},
			&claude.ResultMessage{
				Type:         "result",
				Subtype:      claude.ResultSuccess,
				NumTurns:     5,
				DurationMs:   30000,
				TotalCostUSD: &cost,
			},
		},
	}

	var progress []string
	cfg := &config.Config{
		Agent: config.AgentConfig{
			Model:        "claude-sonnet-4-6",
			MaxTurns:     50,
			MaxBudgetUSD: 5.0,
			AllowedTools: []string{"Bash"},
		},
	}

	r := New(cfg, "/repo", func(update string) {
		progress = append(progress, update)
	})
	r.newSession = func(_ ...claude.Option) session { return mock }

	result, err := r.Run(context.Background(), "Fix the bug")
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if !result.PRCreated {
		t.Error("expected PRCreated to be true")
	}
	if result.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Errorf("PRURL = %q, want %q", result.PRURL, "https://github.com/owner/repo/pull/42")
	}
	if result.NumTurns != 5 {
		t.Errorf("NumTurns = %d, want 5", result.NumTurns)
	}
	if result.CostUSD != 0.50 {
		t.Errorf("CostUSD = %f, want 0.50", result.CostUSD)
	}
	if result.Duration != 30*time.Second {
		t.Errorf("Duration = %v, want 30s", result.Duration)
	}
	if len(progress) < 2 {
		t.Fatalf("expected at least 2 progress updates, got %d: %v", len(progress), progress)
	}
	// First progress should be the task list
	if !strings.Contains(progress[0], "Task List") {
		t.Errorf("first progress update should contain task list, got: %s", progress[0])
	}
	// Last progress should be PR URL
	if !strings.Contains(progress[len(progress)-1], "pull/42") {
		t.Errorf("last progress should contain PR URL, got: %s", progress[len(progress)-1])
	}
}

func TestRunMaxTurns(t *testing.T) {
	mock := &mockSession{
		messages: []claude.Message{
			&claude.ResultMessage{
				Type:     "result",
				Subtype:  claude.ResultErrorMaxTurns,
				NumTurns: 50,
			},
		},
	}

	cfg := &config.Config{Agent: config.AgentConfig{MaxTurns: 50}}
	r := New(cfg, "/repo", nil)
	r.newSession = func(_ ...claude.Option) session { return mock }

	result, err := r.Run(context.Background(), "Fix the bug")
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected Success to be false")
	}
	if result.Error != "exceeded maximum turns" {
		t.Errorf("Error = %q, want %q", result.Error, "exceeded maximum turns")
	}
	if result.NumTurns != 50 {
		t.Errorf("NumTurns = %d, want 50", result.NumTurns)
	}
}

func TestRunMaxBudget(t *testing.T) {
	mock := &mockSession{
		messages: []claude.Message{
			&claude.ResultMessage{
				Type:    "result",
				Subtype: claude.ResultErrorMaxBudget,
			},
		},
	}

	cfg := &config.Config{Agent: config.AgentConfig{MaxBudgetUSD: 5.0}}
	r := New(cfg, "/repo", nil)
	r.newSession = func(_ ...claude.Option) session { return mock }

	result, err := r.Run(context.Background(), "Fix the bug")
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected Success to be false")
	}
	if result.Error != "exceeded budget limit" {
		t.Errorf("Error = %q, want %q", result.Error, "exceeded budget limit")
	}
}

func TestRunExecutionError(t *testing.T) {
	errMsg := "runtime panic in tool"
	mock := &mockSession{
		messages: []claude.Message{
			&claude.ResultMessage{
				Type:    "result",
				Subtype: claude.ResultErrorExecution,
				Result:  &errMsg,
			},
		},
	}

	cfg := &config.Config{}
	r := New(cfg, "/repo", nil)
	r.newSession = func(_ ...claude.Option) session { return mock }

	result, err := r.Run(context.Background(), "Fix the bug")
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected Success to be false")
	}
	if result.Error != "runtime panic in tool" {
		t.Errorf("Error = %q, want %q", result.Error, "runtime panic in tool")
	}
}

func TestRunExecutionErrorNoResult(t *testing.T) {
	mock := &mockSession{
		messages: []claude.Message{
			&claude.ResultMessage{
				Type:    "result",
				Subtype: claude.ResultErrorExecution,
			},
		},
	}

	cfg := &config.Config{}
	r := New(cfg, "/repo", nil)
	r.newSession = func(_ ...claude.Option) session { return mock }

	result, err := r.Run(context.Background(), "Fix the bug")
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	if result.Error != "execution error" {
		t.Errorf("Error = %q, want %q", result.Error, "execution error")
	}
}

func TestRunBeforeHookFailure(t *testing.T) {
	cfg := &config.Config{
		Hooks: config.HooksConfig{BeforeRun: "false"},
	}
	r := New(cfg, ".", nil)

	_, err := r.Run(context.Background(), "Fix the bug")
	if err == nil {
		t.Fatal("expected error from failed before_run hook")
	}
	if !strings.Contains(err.Error(), "before_run hook") {
		t.Errorf("error should mention before_run hook, got: %v", err)
	}
}

func TestRunAfterHookFailure(t *testing.T) {
	mock := &mockSession{
		messages: []claude.Message{
			&claude.ResultMessage{
				Type:    "result",
				Subtype: claude.ResultSuccess,
			},
		},
	}

	cfg := &config.Config{
		Hooks: config.HooksConfig{AfterRun: "false"},
	}
	r := New(cfg, ".", nil)
	r.newSession = func(_ ...claude.Option) session { return mock }

	result, err := r.Run(context.Background(), "Fix the bug")
	if err == nil {
		t.Fatal("expected error from failed after_run hook")
	}
	if !strings.Contains(err.Error(), "after_run hook") {
		t.Errorf("error should mention after_run hook, got: %v", err)
	}
	// Result should still be returned even if after hook fails
	if result == nil {
		t.Fatal("result should be returned even when after_run hook fails")
	}
	if !result.Success {
		t.Error("result.Success should be true despite after_run hook failure")
	}
}

func TestRunSendError(t *testing.T) {
	mock := &mockSession{
		sendErr: claude.ErrCLINotFound,
	}

	cfg := &config.Config{}
	r := New(cfg, "/repo", nil)
	r.newSession = func(_ ...claude.Option) session { return mock }

	_, err := r.Run(context.Background(), "Fix the bug")
	if err == nil {
		t.Fatal("expected error from Send failure")
	}
	if !strings.Contains(err.Error(), "sending prompt") {
		t.Errorf("error should mention sending prompt, got: %v", err)
	}
}

func TestRunNoPR(t *testing.T) {
	cost := 0.10
	mock := &mockSession{
		messages: []claude.Message{
			&claude.AssistantMessage{
				Type: "assistant",
				Message: &claude.APIMessage{
					Content: []claude.ContentBlock{
						&claude.TextBlock{Type: "text", Text: "I've analyzed the code and found no issues."},
					},
				},
			},
			&claude.ResultMessage{
				Type:         "result",
				Subtype:      claude.ResultSuccess,
				NumTurns:     2,
				DurationMs:   5000,
				TotalCostUSD: &cost,
			},
		},
	}

	cfg := &config.Config{}
	r := New(cfg, "/repo", nil)
	r.newSession = func(_ ...claude.Option) session { return mock }

	result, err := r.Run(context.Background(), "Check the code")
	if err != nil {
		t.Fatalf("Run() unexpected error: %v", err)
	}
	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.PRCreated {
		t.Error("expected PRCreated to be false")
	}
	if result.PRURL != "" {
		t.Errorf("PRURL should be empty, got %q", result.PRURL)
	}
}
