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
| runner | `internal/runner/` | TODO | Wraps `claude-agent-sdk-go`, manages single agent session lifecycle |
| orchestrator | `internal/orchestrator/` | TODO | Main loop: watch Plane for "In Progress" cards, dispatch runner, update status |

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

### ngrok
- `Start(ctx, port)` → `*Tunnel` — launches ngrok, waits for HTTPS URL (15s timeout)
- `Tunnel.Stop()` — kills subprocess

## Environment Variables

<!-- AUTO-GENERATED from WORKFLOW.MD.example -->
| Variable | Required | Description | Used In |
|----------|----------|-------------|---------|
| `GITHUB_TOKEN` | Yes | GitHub personal access token for API access | `github.NewClient` |
| `PLANE_API_KEY` | Yes | Plane API key, referenced as `${PLANE_API_KEY}` in WORKFLOW.MD frontmatter | `config.Parse` (env expansion) |
<!-- END AUTO-GENERATED -->

## Key Decisions

- **Single agent at a time** (v1): sequential processing, no concurrency
- **Plane is the UI**: no custom web dashboard, all interaction via Plane Kanban
- **Go Agent SDK**: `github.com/partio-io/claude-agent-sdk-go` for subprocess management
- **Completion detection**: `ResultMessage` subtypes + `PreToolUse` hook for `gh pr create`
- **Task list pattern**: WORKFLOW.MD instructs agent to output numbered tasks first
- **Env var expansion**: `${VAR}` in YAML frontmatter expanded via `os.Expand` before parsing

## Plans & Docs

- [Implementation plan](docs/plans/troupe.md) — phases 0-3 done, 4-6 remaining
- [Harness Engineering spec](docs/plans/harness-engineering.html)

## Dev Commands

<!-- AUTO-GENERATED from go.mod + .golangci.yml -->
| Command | Description |
|---------|-------------|
| `go build ./cmd/troupe/` | Build the troupe binary |
| `go build ./...` | Build all packages |
| `go test ./...` | Run all tests (27 tests across config, github, plane) |
| `go test ./... -v` | Run all tests with verbose output |
| `go vet ./...` | Run Go vet static analysis |
| `golangci-lint run` | Run linter (errcheck, govet, staticcheck, unused, ineffassign, misspell, unconvert, unparam, revive) |
<!-- END AUTO-GENERATED -->

## Dependencies

<!-- AUTO-GENERATED from go.mod -->
| Module | Version | Purpose |
|--------|---------|---------|
| `gopkg.in/yaml.v3` | v3.0.1 | YAML frontmatter parsing in config package |
<!-- END AUTO-GENERATED -->
