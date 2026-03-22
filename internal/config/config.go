// Package config parses WORKFLOW.MD files containing YAML frontmatter and a
// Go template body. The frontmatter configures troupe behaviour while the
// template body becomes the agent prompt.
package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration parsed from a WORKFLOW.MD file.
type Config struct {
	Name   string       `yaml:"name"`
	Repo   string       `yaml:"repo"`
	Agent  AgentConfig  `yaml:"agent"`
	GitHub GitHubConfig `yaml:"github"`
	Plane  PlaneConfig  `yaml:"plane"`
	Hooks  HooksConfig  `yaml:"hooks"`

	// PromptTemplate is the raw Go template text after the YAML frontmatter.
	PromptTemplate string `yaml:"-"`
}

// AgentConfig controls the Claude Code agent session.
type AgentConfig struct {
	Model        string   `yaml:"model"`
	MaxTurns     int      `yaml:"max_turns"`
	MaxBudgetUSD float64  `yaml:"max_budget_usd"`
	AllowedTools []string `yaml:"allowed_tools"`
}

// GitHubConfig controls which issues are fetched.
type GitHubConfig struct {
	ActiveStates []string `yaml:"active_states"`
	Labels       []string `yaml:"labels"`
}

// PlaneConfig holds Plane API connection details and state mapping.
type PlaneConfig struct {
	BaseURL      string `yaml:"base_url"`
	APIKey       string `yaml:"api_key"`
	Workspace    string `yaml:"workspace"`
	Project      string `yaml:"project"`
	TriggerState string `yaml:"trigger_state"`
	DoneState    string `yaml:"done_state"`
	FailedState  string `yaml:"failed_state"`
}

// HooksConfig holds optional shell commands run before/after each agent run.
type HooksConfig struct {
	BeforeRun string `yaml:"before_run"`
	AfterRun  string `yaml:"after_run"`
}

// Issue is the template data passed when rendering the prompt template.
type Issue struct {
	Number int
	Title  string
	Body   string
}

// PromptData is the top-level data available inside the Go template.
type PromptData struct {
	Issue Issue
}

// Load reads a WORKFLOW.MD file and returns a parsed Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow file: %w", err)
	}
	return Parse(data)
}

// Parse splits raw bytes into YAML frontmatter and a Go template body,
// returning the resulting Config. Environment variables in the form ${VAR}
// are expanded in the YAML section before parsing.
func Parse(data []byte) (*Config, error) {
	frontmatter, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}

	// Expand ${VAR} references in YAML frontmatter.
	expanded := os.Expand(string(frontmatter), os.Getenv)

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing YAML frontmatter: %w", err)
	}

	cfg.PromptTemplate = strings.TrimSpace(string(body))
	return &cfg, nil
}

// RenderPrompt executes the prompt template with the given issue data.
func (c *Config) RenderPrompt(issue Issue) (string, error) {
	tmpl, err := template.New("prompt").Parse(c.PromptTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing prompt template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, PromptData{Issue: issue}); err != nil {
		return "", fmt.Errorf("executing prompt template: %w", err)
	}
	return buf.String(), nil
}

// splitFrontmatter separates YAML frontmatter (between --- delimiters) from
// the template body. Returns (frontmatter, body, error).
func splitFrontmatter(data []byte) ([]byte, []byte, error) {
	const delimiter = "---"

	content := string(data)
	content = strings.TrimSpace(content)

	if !strings.HasPrefix(content, delimiter) {
		return nil, nil, fmt.Errorf("workflow file must start with '---' YAML frontmatter delimiter")
	}

	// Find the closing delimiter.
	rest := content[len(delimiter):]
	idx := strings.Index(rest, "\n"+delimiter)
	if idx < 0 {
		return nil, nil, fmt.Errorf("missing closing '---' YAML frontmatter delimiter")
	}

	frontmatter := rest[:idx]
	body := rest[idx+len("\n"+delimiter):]

	return []byte(frontmatter), []byte(body), nil
}
