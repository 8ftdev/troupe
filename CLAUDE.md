# troupe

Claude Code agent orchestrator. Syncs GitHub Issues to Plane Kanban, spawns agents to fix issues one at a time.

## Architecture

Dependency layering (top depends on bottom):
```
cmd/troupe     → CLI entry point (cobra)
orchestrator   → Sequential issue processing loop
runner         → Claude Code subprocess via Go Agent SDK
plane          → Plane REST API client
github         → GitHub REST client + webhook receiver
ngrok          → ngrok tunnel management
config         → WORKFLOW.MD parser + typed config
```

## Package Guide

| Package | Path | Status | Purpose |
|---------|------|--------|---------|
| config | `internal/config/` | DONE | Parses WORKFLOW.MD (YAML frontmatter + Go template body), typed config struct, env var expansion, prompt rendering |
| github | `internal/github/` | DONE | REST client (paginated issue fetch, label filtering, PR filtering), webhook handler (HMAC-SHA256 verification, event channel), webhook registration |
| ngrok | `internal/ngrok/` | DONE | Starts ngrok subprocess, polls local API for HTTPS tunnel URL, graceful stop |
| plane | `internal/plane/` | DONE | REST client (work items CRUD, state resolution, comments), external ID linking for GitHub sync |
| runner | `internal/runner/` | DONE | Wraps `claude-agent-sdk-go`, manages single agent session lifecycle, streams progress, detects PR creation |
| orchestrator | `internal/orchestrator/` | DONE | Sequential processing loop: polls Plane for triggered cards, matches to GitHub issues, runs agent, updates card state |

## Key Types

### config
- `Config` — top-level: Name, Repo, Agent, GitHub, Plane, Hooks, PromptTemplate
- `AgentConfig` — Model, MaxTurns, MaxBudgetUSD, AllowedTools
- `GitHubConfig` — ActiveStates, Labels
- `PlaneConfig` — BaseURL, APIKey, Workspace, Project, TriggerState, DoneState, FailedState
- `HooksConfig` — BeforeRun, AfterRun
- `Issue` / `PromptData` — template rendering data
- `Load(path)` / `Parse(data)` — split frontmatter, expand `${VAR}`, unmarshal YAML
- `Config.RenderPrompt(Issue)` — execute Go template with issue data

### github
- `Issue` — Number, Title, Body, State, Labels, CreatedAt, HTMLURL
- `Event` — Action (`opened`/`edited`/`closed`/`labeled`) + Issue
- `NewClient(token, "owner/repo")` → `*Client`
- `Client.FetchIssues(ctx, labels)` — paginated, sorted by created_at ASC, skips PRs
- `Client.RegisterWebhook(ctx, url, secret)` — creates GitHub webhook
- `WebhookHandler(secret)` — returns `http.Handler` + `<-chan Event`, HMAC-SHA256 verification

### plane
- `State` — ID, Name, Group, Color
- `WorkItem` — ID, Name, Description, StateID, Priority, SequenceID, ExternalSource, ExternalID, CreatedAt, UpdatedAt
- `Comment` — ID, CommentHTML, CreatedAt
- `CreateWorkItemRequest` / `UpdateWorkItemRequest` — request bodies
- `NewClient(apiKey, baseURL, workspace, projectID)` → `*Client`
- `Client.ListStates(ctx)` — fetch all project states
- `Client.ResolveStateID(ctx, name)` — map state name → UUID (cached)
- `Client.ListWorkItems(ctx, stateID)` — list work items, optional state filter
- `Client.CreateWorkItem(ctx, req)` — create work item with external_source/external_id for GitHub linking
- `Client.UpdateWorkItem(ctx, id, req)` — patch work item (state transitions)
- `Client.CreateComment(ctx, workItemID, html)` — post comment on work item
- `Client.FindWorkItemByExternalID(ctx, source, id)` — find linked work item

### runner
- `Result` — Success, PRCreated, PRURL, Error, NumTurns, Duration, CostUSD
- `ProgressFunc` — callback type for streaming progress updates
- `New(cfg, repoDir, onProgress)` → `*Runner` — creates runner from config
- `Runner.Run(ctx, prompt)` → `(*Result, error)` — executes agent session, streams progress, detects PR creation
- Stream processing: extracts task lists from `TextBlock`, detects `gh pr create` from `ToolUseBlock`, captures PR URL from `ToolResultBlock`
- Handles all `ResultMessage` subtypes: `ResultSuccess`, `ResultErrorMaxTurns`, `ResultErrorExecution`, `ResultErrorMaxBudget`
- Runs `hooks.before_run` / `hooks.after_run` shell commands if configured

### orchestrator
- `PlaneClient` / `GitHubClient` / `AgentRunner` — interfaces for testability
- `RunnerFactory` — `func(ProgressFunc) AgentRunner`, creates runner per work item with scoped progress callback
- `New(cfg, plane, github, repoDir, pollInterval, log, newRunner)` → `*Orchestrator`
- `Orchestrator.Run(ctx)` — blocks until ctx cancelled, polls Plane for triggered cards, processes sequentially
- `processNext(ctx)` — finds oldest triggered card, matches GitHub issue, renders prompt, runs agent, updates card state
- `handleResult(ctx, cardID, result)` — moves card to Done (PR created) or Failed (error/no PR)
- `matchGitHubIssue(ctx, card)` — resolves card's external_id to GitHub issue number
- `pickOldest(items)` — sorts by CreatedAt, returns first
- Graceful shutdown: checks `ctx.Done()` at loop top and between polls

### ngrok
- `Start(ctx, port)` → `*Tunnel` — launches ngrok, waits for HTTPS URL (15s timeout)
- `Tunnel.Stop()` — kills subprocess

## Environment Variables

<!-- AUTO-GENERATED from WORKFLOW.MD.example -->
| Variable | Required | Description | Used In |
|----------|----------|-------------|---------|
| `GITHUB_TOKEN` | Yes | GitHub personal access token for API access | `github.NewClient` |
| `PLANE_API_KEY` | Yes | Plane API key, referenced as `${PLANE_API_KEY}` in WORKFLOW.MD frontmatter | `config.Parse` (env expansion) |
| `WEBHOOK_SECRET` | No | HMAC-SHA256 secret for GitHub webhook verification | `github.WebhookHandler` |
<!-- END AUTO-GENERATED -->

## Key Decisions

- **Single agent at a time** (v1): sequential processing, no concurrency
- **Plane is the UI**: no custom web dashboard, all interaction via Plane Kanban
- **Go Agent SDK**: `github.com/partio-io/claude-agent-sdk-go` for subprocess management
- **Completion detection**: `ResultMessage` subtypes + `PreToolUse` hook for `gh pr create`
- **Task list pattern**: WORKFLOW.MD instructs agent to output numbered tasks first
- **Env var expansion**: `${VAR}` in YAML frontmatter expanded via `os.Expand` before parsing
- **npm distribution**: esbuild pattern — main package with platform-specific optional dependencies
- **cobra CLI**: single root command with `--workflow-file` (required), `--poll-interval`, `--webhook-port`

## Plans & Docs

- [Implementation plan](docs/plans/troupe.md) — all phases (0-6) complete
- [Harness Engineering spec](docs/plans/harness-engineering.html)

## Dev Commands

<!-- AUTO-GENERATED from go.mod + .golangci.yml -->
| Command | Description |
|---------|-------------|
| `go build ./cmd/troupe/` | Build the troupe binary |
| `go build ./...` | Build all packages |
| `go test ./...` | Run all tests (61 tests across config, github, plane, runner, orchestrator) |
| `go test ./... -v` | Run all tests with verbose output |
| `go vet ./...` | Run Go vet static analysis |
| `golangci-lint run` | Run linter (errcheck, govet, staticcheck, unused, ineffassign, misspell, unconvert, unparam, revive) |
| `make build` | Build binary with version from git tags |
| `make dist` | Cross-compile for darwin-arm64, darwin-amd64, linux-amd64, linux-arm64 |
| `make test` | Run all tests |
| `make lint` | Run go vet + golangci-lint |
| `scripts/npm-pack.sh <version>` | Build Go binaries and copy into npm platform packages |
<!-- END AUTO-GENERATED -->

## Dependencies

<!-- AUTO-GENERATED from go.mod -->
| Module | Version | Purpose |
|--------|---------|---------|
| `gopkg.in/yaml.v3` | v3.0.1 | YAML frontmatter parsing in config package |
| `github.com/partio-io/claude-agent-sdk-go` | v0.1.0 | Claude Code CLI subprocess management in runner package |
| `github.com/spf13/cobra` | v1.10.2 | CLI framework with flag parsing |
<!-- END AUTO-GENERATED -->
