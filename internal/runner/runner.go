// Package runner wraps the Claude Agent SDK to manage single agent session
// lifecycle, streaming progress and detecting PR creation.
package runner

import (
	"context"
	"fmt"
	"iter"
	"os/exec"
	"regexp"
	"strings"
	"time"

	claude "github.com/partio-io/claude-agent-sdk-go"

	"github.com/8ftdev/troupe/internal/config"
)

// Result represents the outcome of an agent run.
type Result struct {
	Success   bool
	PRCreated bool
	PRURL     string
	Error     string
	NumTurns  int
	Duration  time.Duration
	CostUSD   float64
}

// ProgressFunc is called with progress updates during the agent run.
type ProgressFunc func(update string)

// session abstracts the Claude Agent SDK for testability.
type session interface {
	Send(ctx context.Context, prompt string) error
	Stream(ctx context.Context) iter.Seq2[claude.Message, error]
	Close() error
}

// Runner manages Claude Code agent session lifecycle.
type Runner struct {
	model        string
	maxTurns     int
	maxBudgetUSD float64
	allowedTools []string
	repoDir      string
	beforeRun    string
	afterRun     string
	onProgress   ProgressFunc

	// newSession creates a session. Override for testing.
	newSession func(opts ...claude.Option) session
}

// New creates a Runner configured from the given workflow config.
func New(cfg *config.Config, repoDir string, onProgress ProgressFunc) *Runner {
	if onProgress == nil {
		onProgress = func(string) {}
	}
	return &Runner{
		model:        cfg.Agent.Model,
		maxTurns:     cfg.Agent.MaxTurns,
		maxBudgetUSD: cfg.Agent.MaxBudgetUSD,
		allowedTools: cfg.Agent.AllowedTools,
		repoDir:      repoDir,
		beforeRun:    cfg.Hooks.BeforeRun,
		afterRun:     cfg.Hooks.AfterRun,
		onProgress:   onProgress,
		newSession: func(opts ...claude.Option) session {
			return claude.NewSession(opts...)
		},
	}
}

// Run executes the agent with the given prompt and returns the result.
func (r *Runner) Run(ctx context.Context, prompt string) (*Result, error) {
	if r.beforeRun != "" {
		if err := runHook(ctx, r.repoDir, r.beforeRun); err != nil {
			return nil, fmt.Errorf("before_run hook: %w", err)
		}
	}

	opts := []claude.Option{
		claude.WithModel(r.model),
		claude.WithCwd(r.repoDir),
		claude.WithMaxTurns(r.maxTurns),
		claude.WithMaxBudgetUSD(r.maxBudgetUSD),
		claude.WithAllowedTools(r.allowedTools...),
		claude.WithPermissionMode("bypassPermissions"),
		claude.WithNoSessionPersistence(true),
	}

	sess := r.newSession(opts...)
	defer func() { _ = sess.Close() }()

	if err := sess.Send(ctx, prompt); err != nil {
		return nil, fmt.Errorf("sending prompt: %w", err)
	}

	result := &Result{}
	var pendingPRToolID string

	for msg, err := range sess.Stream(ctx) {
		if err != nil {
			result.Error = err.Error()
			break
		}

		switch m := msg.(type) {
		case *claude.AssistantMessage:
			r.processAssistantMsg(m, &pendingPRToolID)
		case *claude.UserMessage:
			r.processUserMsg(m, result, &pendingPRToolID)
		case *claude.ResultMessage:
			processResultMsg(m, result)
		}
	}

	if r.afterRun != "" {
		if err := runHook(ctx, r.repoDir, r.afterRun); err != nil {
			return result, fmt.Errorf("after_run hook: %w", err)
		}
	}

	return result, nil
}

func (r *Runner) processAssistantMsg(m *claude.AssistantMessage, pendingPRToolID *string) {
	if m.Message == nil {
		return
	}
	for _, block := range m.Message.Content {
		switch b := block.(type) {
		case *claude.TextBlock:
			if tasks := parseTaskList(b.Text); len(tasks) > 0 {
				r.onProgress(formatTaskList(tasks))
			} else if b.Text != "" {
				r.onProgress(b.Text)
			}
		case *claude.ToolUseBlock:
			if detectPRCreation(b.Name, b.Input) {
				*pendingPRToolID = b.ID
			}
		}
	}
}

func (r *Runner) processUserMsg(m *claude.UserMessage, result *Result, pendingPRToolID *string) {
	if m.Message == nil || *pendingPRToolID == "" {
		return
	}
	for _, block := range m.Message.Content {
		tb, ok := block.(*claude.ToolResultBlock)
		if !ok || tb.ToolUseID != *pendingPRToolID {
			continue
		}
		if url := extractPRURL(toolResultContent(tb)); url != "" {
			result.PRCreated = true
			result.PRURL = url
			r.onProgress(fmt.Sprintf("PR created: %s", url))
		}
		*pendingPRToolID = ""
	}
}

func processResultMsg(m *claude.ResultMessage, result *Result) {
	result.NumTurns = m.NumTurns
	result.Duration = time.Duration(m.DurationMs) * time.Millisecond
	if m.TotalCostUSD != nil {
		result.CostUSD = *m.TotalCostUSD
	}
	switch m.Subtype {
	case claude.ResultSuccess:
		result.Success = true
	case claude.ResultErrorMaxTurns:
		result.Error = "exceeded maximum turns"
	case claude.ResultErrorExecution:
		result.Error = "execution error"
		if m.Result != nil {
			result.Error = *m.Result
		}
	case claude.ResultErrorMaxBudget:
		result.Error = "exceeded budget limit"
	}
}

// toolResultContent extracts text from a ToolResultBlock's Content field.
func toolResultContent(tb *claude.ToolResultBlock) string {
	switch v := tb.Content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", v)
	}
}

var prURLPattern = regexp.MustCompile(`https://github\.com/[^/]+/[^/]+/pull/\d+`)
var taskLinePattern = regexp.MustCompile(`^(?:task\s+)?\d+[\.\):\-]\s*(.+)`)

// parseTaskList extracts numbered task items from agent output text.
func parseTaskList(text string) []string {
	var tasks []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if m := taskLinePattern.FindStringSubmatch(line); len(m) > 1 {
			tasks = append(tasks, strings.TrimSpace(m[1]))
		}
	}
	return tasks
}

// formatTaskList formats tasks as an HTML ordered list for Plane comments.
func formatTaskList(tasks []string) string {
	var sb strings.Builder
	sb.WriteString("<p><strong>Task List</strong></p><ol>")
	for _, task := range tasks {
		sb.WriteString("<li>")
		sb.WriteString(task)
		sb.WriteString("</li>")
	}
	sb.WriteString("</ol>")
	return sb.String()
}

// detectPRCreation returns true if the tool call is a gh pr create command.
func detectPRCreation(toolName string, input map[string]any) bool {
	if toolName != "Bash" {
		return false
	}
	cmd, ok := input["command"].(string)
	return ok && strings.Contains(cmd, "gh pr create")
}

// extractPRURL finds a GitHub PR URL in text.
func extractPRURL(text string) string {
	return prURLPattern.FindString(text)
}

// runHook executes a shell command in the given directory.
func runHook(ctx context.Context, dir, command string) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
}
