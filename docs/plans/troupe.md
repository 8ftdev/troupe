### troupe

troupe is a Claude Code agent orchestrator connected to GitHub Issues through Plane's Kanban dashboard.

Users run the tool `npx troupe --workflow-file ./WORKFLOW.MD` and a headless Go daemon starts. Issues from a GitHub repository are synced to a Plane Kanban board. When a user moves an issue card to "In Progress" in Plane, troupe spawns a Claude Code agent to work on it. Agent progress is posted as comments on the Plane card. Issues are processed one at a time, sequentially, sorted by creation date.

The project follows Symphony spec: https://github.com/openai/symphony/blob/main/SPEC.md with tweaks: Claude Code instead of Codex, GitHub Issues instead of Linear, Plane as the UI.

## Tech

- **Language**: Go 1.26.1
- **Module**: `github.com/8ftdev/troupe`
- **Agent SDK**: `github.com/partio-io/claude-agent-sdk-go` (Go wrapper around Claude CLI subprocess)
- **Dashboard**: Plane (self-hosted, user-managed) via REST API
- **Issue Source**: GitHub Issues via REST API + webhooks (ngrok-exposed)
- **Distribution**: Pre-built Go binaries via npm (like esbuild/turbo)
- **Dependencies**: `gopkg.in/yaml.v3` (WORKFLOW.MD frontmatter parsing)
- **Linting**: golangci-lint v2 (errcheck, govet, staticcheck, revive, misspell, +4 more)

Reference repos:
- https://github.com/jayminwest/overstory
- https://github.com/penso/arbor
- https://github.com/makeplane/plane
- https://github.com/openai/symphony

Development flow per feature:
review requirements -> plan -> audit plan -> tdd-workflow -> audit changes -> generate PR

This project follows "Harness Engineering" methodology (check harness-engineering.html for the spec), strictly documenting every feature and using CLAUDE.md as an index for other agents to keep a cleaner context and reduce agent entropy.

---

## Architecture

```
GitHub Issues ──webhook(ngrok)──> troupe (Go daemon) ──REST API──> Plane Kanban
                                       │
                                       │ spawns 1 agent at a time
                                       ▼
                                  Claude Code CLI
                                  (via Go Agent SDK)
                                       │
                                       │ streams events
                                       ▼
                                  Plane card comments
                                  (real-time progress)
```

### Core Flow

1. troupe starts, fetches open GitHub issues sorted by `created_at ASC`
2. Syncs issues to Plane Kanban board (column = issue state)
3. Watches for Plane card state changes (polling or webhook)
4. When a card moves to "In Progress" → spawn Claude Code agent for that issue
5. Agent works in the repo directory, streams progress
6. Agent output posted as Plane card comments (task list, progress, errors)
7. On completion (PR created) or error → move card to "Done" or "Failed"
8. Pick next issue only after current agent finishes

### Agent Completion Detection

Using the Go Agent SDK (`github.com/partio-io/claude-agent-sdk-go`):

- **ResultMessage** signals completion with subtypes:
  - `ResultSuccess` — agent completed (expect PR created)
  - `ResultErrorMaxTurns` — exceeded turn limit
  - `ResultErrorExecution` — runtime error
  - `ResultErrorMaxBudget` — cost limit hit
- **Hook interception** (`WithHook(PreToolUse)`) detects `Bash(gh pr create ...)` calls to confirm PR creation
- **Stream events** (`AssistantMessage`, `ToolUseBlock`) provide real-time progress for Plane comments
- **Safety limits**: `WithMaxTurns(n)` and `WithMaxBudgetUSD(budget)` prevent runaway agents

### Agent Task List Pattern (via WORKFLOW.MD)

The WORKFLOW.MD prompt instructs agents to:
1. Analyze the issue and output a numbered task list as the first step
2. Work through tasks sequentially, reporting progress after each
3. Create a PR when all tasks pass
4. Signal completion

Example for issue "Register form submit button has a typo":
```
task 1: write test for button text
task 2: update code
task 3: check test pass
task 4: create PR with fixes
```

Each task completion is posted as a Plane card comment.

---

## Project Structure (Harness Engineering)

```
troupe/
├── CLAUDE.md                          # Index/map for agent context
├── WORKFLOW.MD.example                # Example workflow file
├── .gitignore                         # Ignores binaries, .env, vendor, IDE files
├── .golangci.yml                      # Linter config (v2)
├── go.mod / go.sum                    # Go module (gopkg.in/yaml.v3)
├── cmd/
│   └── troupe/main.go                 # CLI entry point
├── internal/
│   ├── config/                        # [DONE] WORKFLOW.MD parser + typed config
│   │   ├── config.go                  #   Load/Parse, env expansion, RenderPrompt
│   │   └── config_test.go             #   7 tests
│   ├── github/                        # [DONE] GitHub REST client + webhooks
│   │   ├── issue.go                   #   Issue/Event domain types
│   │   ├── client.go                  #   NewClient, FetchIssues, RegisterWebhook
│   │   ├── client_test.go             #   4 tests
│   │   ├── webhook.go                 #   WebhookHandler, HMAC-SHA256 verification
│   │   └── webhook_test.go            #   5 tests
│   ├── ngrok/                         # [DONE] ngrok tunnel management
│   │   └── ngrok.go                   #   Start/Stop, polls local API (15s timeout)
│   ├── plane/                         # [TODO] Plane REST API client
│   ├── runner/                        # [TODO] Claude Code subprocess via Go SDK
│   └── orchestrator/                  # [TODO] Sequential issue processing loop
└── docs/
    └── plans/                         # This file, harness-engineering spec
```

Dependency layering: `config -> github -> plane -> runner -> orchestrator -> cmd`

---

## Implementation Plan (v1)

### Phase 0: Project Scaffolding
**Complexity: LOW**

- [x] Initialize Go module
- [x] Set up project structure per above
- [x] Create CLAUDE.md index
- [x] Set up golangci-lint
- [x] Create WORKFLOW.MD.example

### Phase 1: Config & Workflow Loader
**Complexity: LOW**

- [x] Parse WORKFLOW.MD with YAML frontmatter + Go template body
- [x] Typed config struct:
  ```yaml
  ---
  name: "my-project"
  repo: "owner/repo"
  agent:
    model: "claude-sonnet-4-6"
    max_turns: 50
    max_budget_usd: 5.00
    allowed_tools: ["Bash", "Read", "Edit", "Write", "Glob", "Grep"]
  github:
    active_states: ["open"]
    labels: ["agent-ready"]
  plane:
    base_url: "http://localhost:8080"
    workspace: "my-workspace"
    project: "my-project"
    trigger_state: "In Progress"
    done_state: "Done"
    failed_state: "Failed"
  hooks:
    before_run: "./scripts/setup.sh"
    after_run: "./scripts/cleanup.sh"
  ---

  You are an agent working on issue #{{.Issue.Number}}: {{.Issue.Title}}

  {{.Issue.Body}}

  IMPORTANT: Before writing any code, output a numbered task list of steps
  you will take to resolve this issue. Then work through each task, reporting
  progress after each step. When all tasks pass, create a PR with your changes and a reference to the original issue.
  ```
- [x] Environment variable overrides for secrets (GITHUB_TOKEN, PLANE_API_KEY)

### Phase 2: GitHub Client
**Complexity: MEDIUM**

- [x] REST client to fetch open issues (filtered by labels, sorted by `created_at ASC`)
- [x] Webhook HTTP endpoint (`POST /webhooks/github`)
- [x] ngrok tunnel auto-start + webhook URL registration
- [x] Event handling: `issues.opened`, `issues.edited`, `issues.closed`, `issues.labeled`
- [x] Normalize to internal `Issue` struct

### Phase 3: Plane Client
**Complexity: MEDIUM**

- [ ] Plane REST API client (create/update/list work items)
- [ ] Sync GitHub issues to Plane Kanban cards
- [ ] Map states: Open -> Todo, In Progress -> In Progress, Closed -> Done
- [ ] Detect card moved to "In Progress" (poll Plane API for state changes)
- [ ] Post comments on cards (agent task list, progress updates, errors, PR link)
- [ ] Move cards to "Done" or "Failed" on agent completion

### Phase 4: Agent Runner
**Complexity: HIGH**

- [ ] Use `github.com/partio-io/claude-agent-sdk-go`:
  ```go
  session := claude.NewSession(
      claude.WithModel(cfg.Agent.Model),
      claude.WithCwd(repoDir),
      claude.WithSystemPrompt(assembledPrompt),
      claude.WithMaxTurns(cfg.Agent.MaxTurns),
      claude.WithMaxBudgetUSD(cfg.Agent.MaxBudgetUSD),
      claude.WithAllowedTools(cfg.Agent.AllowedTools...),
      claude.WithHook(claude.HookPreToolUse, detectPRCreation),
  )
  defer session.Close()

  session.Send(ctx, issuePrompt)
  for msg, err := range session.Stream(ctx) {
      // Post progress to Plane card comments
      // Detect task list output
      // Detect PR creation via hook
      // Handle ResultMessage for completion
  }
  ```
- [ ] Parse agent output to extract task list and progress
- [ ] Detect PR creation via `PreToolUse` hook on `Bash(gh pr create ...)`
- [ ] Post updates to Plane card as comments
- [ ] Handle all result subtypes (success, max turns, error, max budget)
- [ ] Run `hooks.before_run` and `hooks.after_run` if configured

### Phase 5: Orchestrator Loop
**Complexity: MEDIUM**

- [ ] Sequential processing loop:
  ```
  loop:
    issues = github.FetchOpenIssues(sortBy: created_at ASC)
    planeCards = plane.GetCards(state: trigger_state)
    card = pickNextTriggered(planeCards)  // oldest first
    if card == nil { sleep(pollInterval); continue }

    issue = matchGitHubIssue(card)
    plane.PostComment(card, "Agent starting...")
    result = runner.Run(ctx, issue)

    if result.Success && result.PRCreated:
      plane.MoveCard(card, "Done")
      plane.PostComment(card, "PR created: " + result.PRURL)
    else:
      plane.MoveCard(card, "Failed")
      plane.PostComment(card, "Error: " + result.Error)

    continue  // next issue
  ```
- [ ] Graceful shutdown (SIGINT/SIGTERM stops after current agent finishes)
- [ ] CLI output: structured logging of current issue, agent status, queue

### Phase 6: CLI & npm Distribution
**Complexity: MEDIUM**

- [ ] cobra CLI with `--workflow-file` flag
- [ ] Pre-built Go binaries for darwin-arm64, darwin-amd64, linux-amd64, linux-arm64
- [ ] npm package with platform-specific optional dependencies (esbuild pattern)
- [ ] `npx troupe --workflow-file ./WORKFLOW.MD` downloads + runs correct binary

---

## Risks

| Severity | Risk | Mitigation |
|----------|------|------------|
| HIGH | Agent never finishes (hangs) | `WithMaxTurns` + `WithMaxBudgetUSD` as hard limits |
| HIGH | Claude CLI output format changes between versions | Go SDK abstracts this; pin SDK version |
| MEDIUM | Plane API rate limits or auth complexity | Cache state locally, batch updates |
| MEDIUM | ngrok dependency for webhooks | Support polling fallback (30s GitHub API poll) |
| MEDIUM | Agent makes breaking changes to repo | `hooks.after_run` can run tests/git reset |
| LOW | Sequential processing is slow for many issues | Accepted for v1 |

---

## v2 TODO (Concurrency & Advanced Features)

- [ ] `conc` worker pool for parallel agent execution (N agents simultaneously)
- [ ] Git worktree isolation per agent (each agent works in its own worktree)
- [ ] Worktree locking/mutex to prevent conflicts
- [ ] Per-state concurrency limits (e.g., max 2 in "Todo", max 1 in "In Progress")
- [ ] Retry queue with exponential backoff (Symphony model)
- [ ] Auto-dispatch mode (Symphony-style polling: auto-start agents for eligible issues)
- [ ] Agent-to-agent handoff (reviewer agent after builder agent)
- [ ] Plane webhook receiver (instead of polling for card state changes)
- [ ] Dashboard metrics: tokens used, cost per issue, success rate
- [ ] Multiple repo support
