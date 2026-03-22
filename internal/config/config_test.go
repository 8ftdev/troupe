package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleWorkflow = `---
name: "my-project"
repo: "owner/repo"
agent:
  model: "claude-sonnet-4-6"
  max_turns: 50
  max_budget_usd: 5.00
  allowed_tools:
    - "Bash"
    - "Read"
    - "Edit"
github:
  active_states:
    - "open"
  labels:
    - "agent-ready"
plane:
  base_url: "http://localhost:8080"
  api_key: "pk_test_123"
  workspace: "my-workspace"
  project: "my-project"
  trigger_state: "In Progress"
  done_state: "Done"
  failed_state: "Failed"
hooks:
  before_run: "./scripts/setup.sh"
  after_run: ""
---

You are an agent working on issue #{{.Issue.Number}}: {{.Issue.Title}}

{{.Issue.Body}}
`

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(sampleWorkflow))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if cfg.Name != "my-project" {
		t.Errorf("Name = %q, want %q", cfg.Name, "my-project")
	}
	if cfg.Repo != "owner/repo" {
		t.Errorf("Repo = %q, want %q", cfg.Repo, "owner/repo")
	}
	if cfg.Agent.Model != "claude-sonnet-4-6" {
		t.Errorf("Agent.Model = %q, want %q", cfg.Agent.Model, "claude-sonnet-4-6")
	}
	if cfg.Agent.MaxTurns != 50 {
		t.Errorf("Agent.MaxTurns = %d, want %d", cfg.Agent.MaxTurns, 50)
	}
	if cfg.Agent.MaxBudgetUSD != 5.0 {
		t.Errorf("Agent.MaxBudgetUSD = %f, want %f", cfg.Agent.MaxBudgetUSD, 5.0)
	}
	if len(cfg.Agent.AllowedTools) != 3 {
		t.Errorf("Agent.AllowedTools length = %d, want 3", len(cfg.Agent.AllowedTools))
	}
	if cfg.Plane.TriggerState != "In Progress" {
		t.Errorf("Plane.TriggerState = %q, want %q", cfg.Plane.TriggerState, "In Progress")
	}
	if cfg.Hooks.BeforeRun != "./scripts/setup.sh" {
		t.Errorf("Hooks.BeforeRun = %q, want %q", cfg.Hooks.BeforeRun, "./scripts/setup.sh")
	}
	if cfg.PromptTemplate == "" {
		t.Error("PromptTemplate is empty")
	}
}

func TestParse_EnvExpansion(t *testing.T) {
	t.Setenv("PLANE_API_KEY", "secret_key_from_env")

	input := `---
name: "test"
repo: "owner/repo"
plane:
  api_key: "${PLANE_API_KEY}"
  base_url: "http://localhost"
  workspace: "ws"
  project: "proj"
  trigger_state: "In Progress"
  done_state: "Done"
  failed_state: "Failed"
---

prompt
`
	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if cfg.Plane.APIKey != "secret_key_from_env" {
		t.Errorf("Plane.APIKey = %q, want %q", cfg.Plane.APIKey, "secret_key_from_env")
	}
}

func TestParse_MissingFrontmatter(t *testing.T) {
	_, err := Parse([]byte("no frontmatter here"))
	if err == nil {
		t.Error("expected error for missing frontmatter, got nil")
	}
}

func TestParse_MissingClosingDelimiter(t *testing.T) {
	_, err := Parse([]byte("---\nname: test\nno closing"))
	if err == nil {
		t.Error("expected error for missing closing delimiter, got nil")
	}
}

func TestRenderPrompt(t *testing.T) {
	cfg, err := Parse([]byte(sampleWorkflow))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	issue := Issue{
		Number: 42,
		Title:  "Fix the button",
		Body:   "The submit button has a typo.",
	}

	result, err := cfg.RenderPrompt(issue)
	if err != nil {
		t.Fatalf("RenderPrompt() error: %v", err)
	}

	if got := result; got == "" {
		t.Fatal("RenderPrompt() returned empty string")
	}

	// Check that template variables were substituted.
	for _, want := range []string{"#42", "Fix the button", "The submit button has a typo."} {
		if !contains(result, want) {
			t.Errorf("RenderPrompt() result missing %q", want)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.MD")
	if err := os.WriteFile(path, []byte(sampleWorkflow), 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Name != "my-project" {
		t.Errorf("Name = %q, want %q", cfg.Name, "my-project")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/WORKFLOW.MD")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
