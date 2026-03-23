# Architecture

This document describes the high-level architecture of troupe.
If you want to familiarize yourself with the code base, you are in the right place.

## Bird's Eye View

troupe is a long-running daemon that bridges two systems — GitHub and Plane — and spawns Claude Code agents to resolve issues autonomously.

```
  GitHub                              troupe                              Plane
 ┌──────────┐  webhook (ngrok)   ┌──────────────┐   REST API        ┌──────────┐
 │  Issues   │──────────────────>│  sync layer   │─────────────────>│  Kanban   │
 │  (source  │  issues events    │               │  create/update   │  board    │
 │  of work) │                   │               │  work items      │  (UI)     │
 └──────────┘                    │               │                  └────┬─────┘
                                 │               │                       │
                                 │  orchestrator │<──────────────────────┘
                                 │               │  poll for "In Progress" cards
                                 │               │
                                 │    runner     │
                                 │      │        │
                                 │      ▼        │
                                 │  Claude Code  │
                                 │  subprocess   │
                                 └──────────────┘
```

On the highest level, the system has two concurrent concerns:

1. **Sync**: GitHub issues flow into Plane as Kanban cards. This happens via an initial bulk sync at startup, then incrementally via webhooks. The user never interacts with GitHub for triage — Plane is the single UI.

2. **Orchestrate**: The daemon polls Plane for cards in the "In Progress" state. When it finds one, it matches it back to the original GitHub issue, renders a prompt from a Go template, spawns a Claude Code agent, and moves the card to "Done" or "Failed" depending on whether the agent created a PR.

These two concerns are deliberately not interleaved: the sync layer writes cards, the orchestrator reads them. Plane is the coordination point. A human moves a card to "In Progress" to trigger agent work — troupe never auto-starts agents.

**Architecture Invariant:** troupe processes one agent at a time. There is no concurrency in the orchestrator loop. This is a v1 simplification that avoids git conflicts, worktree management, and resource contention. The sequential model is correct by construction.


## Data Flow

A complete lifecycle of an issue looks like this:

```
1. Issue opened on GitHub
       │
       ▼
2. Webhook event (or initial sync) creates Plane card
   with external_source="github", external_id="42"
       │
       ▼
3. Human reviews card in Plane, moves to "In Progress"
       │
       ▼
4. Orchestrator polls, finds triggered card
       │
       ▼
5. matchGitHubIssue: parse external_id → fetch issue #42
       │
       ▼
6. RenderPrompt: execute Go template with issue data
       │
       ▼
7. Runner: spawn Claude Code subprocess
   - streams progress → posts comments on Plane card
   - detects `gh pr create` → captures PR URL
       │
       ▼
8. handleResult:
   - PR created? → move card to "Done", comment with PR link
   - No PR / error? → move card to "Failed", comment with error
```

The external_id linkage is the critical thread that connects a Plane card back to its GitHub issue. Without it, the orchestrator cannot render a prompt. The sync layer is responsible for setting this link when creating cards.

**Architecture Invariant:** every Plane card that troupe creates has `external_source="github"` and `external_id=<issue number as string>`. This is the only mechanism for matching cards to issues. If a card lacks this link (e.g., manually created in Plane), the orchestrator skips it with an error.


## Code Map

This section describes each package, what it does, why it exists, and what invariants it maintains.

The dependency layering looks like this (top depends on bottom):

```
cmd/troupe         → CLI entry point, wires everything together
  └─ orchestrator  → sequential processing loop
       ├─ runner   → Claude Code subprocess management
       ├─ plane    → Plane REST API client
       └─ github   → GitHub REST client + webhook receiver
            └─ ngrok → ngrok tunnel for webhooks
  └─ config        → WORKFLOW.MD parser (used by everyone)
```

No package imports a sibling at the same layer. `config` is at the bottom and knows nothing about the other packages. `orchestrator` depends on `runner`, `plane`, and `github` only through interfaces.


### `internal/config/`

**Files:** `config.go` (140 lines), `config_test.go` (7 tests)

This package owns the `WORKFLOW.MD` file format — a Markdown file with YAML frontmatter and a Go template body. It is the single source of truth for how troupe behaves.

The frontmatter is a YAML block between `---` delimiters that configures:
- which repository to watch (`repo: "owner/repo"`)
- agent parameters (`model`, `max_turns`, `max_budget_usd`, `allowed_tools`)
- GitHub filters (`active_states`, `labels`)
- Plane connection (`base_url`, `api_key`, `workspace`, `project`, `trigger_state`, `done_state`, `failed_state`)
- lifecycle hooks (`before_run`, `after_run`)

The body after the closing `---` is a Go `text/template` that becomes the agent's prompt. It receives a `PromptData` struct containing the issue's number, title, and body.

`Parse(data)` splits frontmatter from body, expands `${VAR}` references via `os.Expand` (so secrets like `${PLANE_API_KEY}` never appear in the file), unmarshals YAML, and stores the raw template text. `Load(path)` is the file-reading wrapper. `RenderPrompt(issue)` executes the template.

**Architecture Invariant:** `config` does no I/O beyond reading the workflow file. It does not call APIs, start processes, or touch the filesystem. Every other package receives a `*Config` and reads what it needs.

**Architecture Invariant:** environment variable expansion happens only in the YAML frontmatter, not in the template body. The template receives structured data via `PromptData`, not raw strings. This means secrets in env vars can appear in YAML config fields but will never leak into rendered prompts unless explicitly wired through the template.


### `internal/github/`

**Files:** `issue.go` (23 lines), `client.go` (141 lines), `webhook.go` (167 lines), `client_test.go` (4 tests), `webhook_test.go` (5 tests)

This package provides two things: a REST client for fetching issues, and an HTTP handler for receiving webhook events.

**`issue.go`** defines two domain types: `Issue` (Number, Title, Body, State, Labels, CreatedAt, HTMLURL) and `Event` (Action + Issue). These are troupe's canonical representation, decoupled from the GitHub API's JSON shape. The `apiIssue` struct in `client.go` handles JSON mapping and is not exported.

**`client.go`** implements `NewClient(token, "owner/repo")` which parses the owner/repo string and creates an HTTP client with a 30-second timeout. `FetchIssues(ctx, labels)` paginates through the GitHub REST API (`per_page=100`), filters out pull requests (which GitHub's issues endpoint also returns), and sorts results by `created_at` ascending. The ascending sort matters: the orchestrator processes the oldest card first, so issues arrive in Plane in chronological order.

`RegisterWebhook(ctx, url, secret)` creates a GitHub webhook pointing at the given URL. This is called once at startup after the ngrok tunnel is established.

**`webhook.go`** implements `WebhookHandler(secret)` which returns an `http.Handler` and a `<-chan Event`. The handler:
1. Rejects non-POST requests
2. Ignores non-`issues` event types (ping, push, etc.)
3. Verifies HMAC-SHA256 signature if a secret is configured
4. Parses the JSON payload and normalises it to an `Event`
5. Sends the event to the channel (non-blocking; drops if the channel buffer of 64 is full)

Only four actions are forwarded: `opened`, `edited`, `closed`, `labeled`. Everything else is silently dropped.

**Architecture Invariant:** the webhook handler never blocks. The channel send is wrapped in a `select` with a `default` case. If the consumer falls behind, events are dropped rather than building backpressure. This prevents a slow Plane API from blocking GitHub's webhook delivery, which would cause retries and potential duplicates.

**Architecture Invariant:** `github` knows nothing about Plane. It produces `Event` values; the CLI (`cmd/troupe`) decides what to do with them. The package is reusable without Plane.


### `internal/plane/`

**Files:** `types.go` (53 lines), `client.go` (233 lines), `client_test.go` (11 tests)

This package is a REST client for Plane's project management API, scoped to a single workspace/project.

**`types.go`** defines the domain types: `State` (ID, Name, Group, Color), `WorkItem` (with ExternalSource/ExternalID for GitHub linking), `Comment`, and request bodies (`CreateWorkItemRequest`, `UpdateWorkItemRequest`).

**`client.go`** implements CRUD operations:
- `ListStates` / `ResolveStateID` — fetches all project states and caches the name→ID mapping after the first call. The cache is never invalidated (states rarely change during a daemon's lifetime).
- `ListWorkItems(ctx, stateID)` — lists work items, optionally filtered by state. Used by the orchestrator to find triggered cards.
- `CreateWorkItem` / `UpdateWorkItem` — create and patch work items. The create request includes `ExternalSource` and `ExternalID` fields for GitHub linking.
- `CreateComment` — posts HTML comments on cards. Used by the orchestrator to stream agent progress.
- `FindWorkItemByExternalID` — scans all work items to find one matching a given source/ID pair. This is O(n) over all items because Plane's API doesn't support filtering by external_id directly.

All methods go through `doRequest`, which sets the `X-API-Key` header and `Content-Type: application/json`. The Plane API uses a paginated response envelope (`{ "results": [...] }`), decoded by a generic `paginatedResponse[T]` type.

**Architecture Invariant:** the Plane client is stateless except for the state name cache. It does not track which cards exist or what state they are in. Each operation makes a fresh API call. This means the orchestrator always sees the current state of the board, never a stale snapshot.

**Architecture Invariant:** `plane` knows nothing about GitHub. It operates on generic work items with generic external_source/external_id fields. The "github" source value is a convention established by the CLI layer, not by this package.


### `internal/ngrok/`

**Files:** `ngrok.go` (94 lines), no tests (subprocess-dependent)

This package manages an ngrok subprocess to expose a local HTTP port to the internet. GitHub cannot deliver webhooks to `localhost`, so ngrok provides a public HTTPS URL that tunnels back to the local webhook server.

`Start(ctx, port)` launches `ngrok http <port> --log=stderr`, then polls ngrok's local API (`http://127.0.0.1:4040/api/tunnels`) every 500ms until an HTTPS tunnel URL appears, with a 15-second timeout. It returns a `*Tunnel` with the public URL. `Tunnel.Stop()` kills the process.

**Architecture Invariant:** ngrok is an optional infrastructure concern, not a core dependency. The webhook handler works with any publicly reachable URL. In production, you might use a cloud load balancer instead of ngrok. The CLI couples them for convenience in local development, but the `github` and `orchestrator` packages have no awareness of ngrok at all.


### `internal/runner/`

**Files:** `runner.go` (259 lines), `runner_test.go` (16 tests)

This is the bridge between troupe and Claude Code. It wraps the `claude-agent-sdk-go` library to manage a single agent session: send a prompt, stream the response, detect tool usage, and capture the outcome.

**Session lifecycle:** `New(cfg, repoDir, onProgress)` creates a `Runner` from config. `Run(ctx, prompt)` is the main entry point:

1. Execute `hooks.before_run` shell command if configured
2. Create a Claude session with options: model, working directory, max turns, max budget, allowed tools, bypass permissions, no session persistence
3. Send the prompt
4. Stream messages until completion
5. Execute `hooks.after_run` shell command if configured
6. Return a `Result`

**Stream processing** is the interesting part. The Claude SDK yields three message types:

- `AssistantMessage` — contains the agent's text output and tool calls. The runner scans `TextBlock` content for numbered task lists (regex: `^\d+[.):−]\s*(.+)`) and formats them as HTML for Plane comments via `onProgress`. It also watches `ToolUseBlock` for Bash commands containing `gh pr create`, recording the tool call ID.
- `UserMessage` — contains tool results. When the runner has a pending PR tool ID, it scans `ToolResultBlock` for a GitHub PR URL (regex: `https://github\.com/[^/]+/[^/]+/pull/\d+`). If found, it sets `result.PRCreated = true` and captures the URL.
- `ResultMessage` — the session's final status. It contains turn count, duration, and cost. The `Subtype` field maps to the outcome: `ResultSuccess`, `ResultErrorMaxTurns`, `ResultErrorExecution`, or `ResultErrorMaxBudget`.

**PR detection** uses a two-phase approach:
1. When the agent calls `Bash` with a command containing `gh pr create`, the runner records the tool call's ID (this is a `ToolUseBlock` in an `AssistantMessage`).
2. When the corresponding `ToolResultBlock` arrives (in the next `UserMessage`), the runner extracts the PR URL from the output text.

This two-phase approach is necessary because the PR URL doesn't exist until the tool call completes. The tool call ID links the request to its response across separate messages.

**Architecture Invariant:** the runner creates one Claude subprocess per invocation. There is no session reuse or pooling. Each `Run` call starts fresh with a clean session. This guarantees isolation between agent runs — no state leaks between issues.

**Architecture Invariant:** the runner does not import `plane` or know about Kanban cards. Progress updates go through the `ProgressFunc` callback, which the orchestrator wires to Plane comments. The runner would work identically with a different callback (e.g., printing to stdout).

**Architecture Invariant:** `runHook` executes shell commands via `sh -c` in the repository directory. Hooks run synchronously — the agent does not start until `before_run` completes, and `after_run` runs after the agent finishes regardless of success. A hook failure aborts the run.


### `internal/orchestrator/`

**Files:** `orchestrator.go` (238 lines), `orchestrator_test.go` (18 tests)

This is the heart of troupe — the sequential processing loop that turns Plane cards into agent runs.

**Interfaces.** The orchestrator depends on three interfaces, not concrete types:

```go
type PlaneClient interface {
    ResolveStateID(ctx, name) (string, error)
    ListWorkItems(ctx, stateID) ([]WorkItem, error)
    UpdateWorkItem(ctx, id, req) (*WorkItem, error)
    CreateComment(ctx, workItemID, html) (*Comment, error)
}

type GitHubClient interface {
    FetchIssues(ctx, labels) ([]Issue, error)
}

type AgentRunner interface {
    Run(ctx, prompt) (*Result, error)
}
```

This makes the orchestrator fully testable with mock implementations. The test file has 18 tests covering the complete state machine without any real API calls or subprocesses.

**RunnerFactory.** The orchestrator doesn't hold a single runner — it receives a `RunnerFactory` function: `func(ProgressFunc) AgentRunner`. A new runner is created per work item, with a progress callback scoped to that card's ID. This ensures that progress comments go to the right card, even if the runner is stateless.

**The main loop** (`Run(ctx)`) is deliberately simple:

```
for {
    check ctx.Done() → return
    processed, err := processNext(ctx)
    if !processed → sleep(pollInterval), check ctx.Done()
}
```

**`processNext`** does the real work:
1. Resolve the trigger state name ("In Progress") to a Plane UUID
2. List all work items in that state
3. Pick the oldest one (`pickOldest` sorts by `CreatedAt`)
4. Parse `external_id` → match to a GitHub issue
5. Render the prompt template with issue data
6. Post "Agent starting..." comment
7. Create a runner with a scoped progress callback
8. Run the agent
9. Handle the result: move card to "Done" if PR created, "Failed" otherwise

**State transitions.** Cards move through this state machine:

```
[trigger_state] → agent runs → [done_state]    (success + PR)
                             → [failed_state]   (error, no PR, or timeout)
```

The state names are configurable in the workflow file. The orchestrator uses `ResolveStateID` to map names to Plane UUIDs, which are cached by the Plane client.

**Architecture Invariant:** the orchestrator processes exactly one card per `processNext` call. It always picks the oldest. If multiple cards are in "In Progress", they are processed sequentially, oldest first. This is FIFO by creation time.

**Architecture Invariant:** the orchestrator never creates cards. It only reads and transitions existing cards. Card creation is the exclusive responsibility of the sync layer in `cmd/troupe`.

**Architecture Invariant:** a failed match (no external_id, issue not found, render error) moves the card to "Failed" with an error comment. The orchestrator never leaves a card in "In Progress" after attempting to process it. Every card either succeeds or fails — there are no stuck cards.


### `cmd/troupe/`

**Files:** `main.go` (308 lines), no tests (integration-level)

This is the entry point that wires everything together. It is the only package that imports all internal packages and the only one that performs I/O setup (environment variables, HTTP server, ngrok, signal handling).

**CLI.** Uses cobra with a single root command:
- `--workflow-file` / `-f` (required): path to WORKFLOW.MD
- `--poll-interval` (default 30s): orchestrator poll frequency
- `--webhook-port` (default 8090): local HTTP port for webhooks

**Startup sequence** (`run` function):
1. Load config from workflow file
2. Validate GITHUB_TOKEN and Plane API key
3. Create GitHub and Plane clients
4. Register signal handler (SIGINT/SIGTERM via `signal.NotifyContext`)
5. **Initial sync**: fetch all matching GitHub issues, create Plane cards for new ones (skip existing by checking `FindWorkItemByExternalID`)
6. Start HTTP server for webhooks on the configured port
7. Start ngrok tunnel, register GitHub webhook
8. Start background goroutine: webhook events → Plane card CRUD
9. Create runner factory
10. Start orchestrator loop (blocks until context cancelled)
11. On shutdown: stop HTTP server, stop ngrok

**Sync layer** lives in this file as two functions:

`syncIssuesToPlane` handles the initial bulk sync. For each GitHub issue matching the configured labels, it checks if a Plane card already exists (via `FindWorkItemByExternalID`). If not, it creates one with `external_source="github"` and `external_id=<issue number>`. Errors are logged and skipped — a single failed card doesn't abort the sync.

`handleWebhookEvent` handles ongoing incremental sync. It processes four event types:
- `opened` → create card (if not exists)
- `edited` → update card name/description (if exists)
- `closed` → move card to done state (if exists)
- `labeled` → create card (if not exists AND labels match filter)

**Architecture Invariant:** `cmd/troupe` is the only package that reads environment variables directly. All other packages receive their configuration through constructor arguments or the `*Config` struct.

**Architecture Invariant:** the webhook event handler is non-blocking and runs in a background goroutine. It does not interact with the orchestrator. The only shared state between the sync goroutine and the orchestrator is the Plane API — they communicate indirectly through card state. This means a webhook event can create a card while the orchestrator is processing a different one, and the orchestrator will pick it up on the next poll.

**Architecture Invariant:** shutdown is orderly. `signal.NotifyContext` cancels the context, which causes the orchestrator's `Run` loop to exit at the next `ctx.Done()` check. The deferred cleanup stops the HTTP server (with a 5-second grace period) and kills the ngrok process. If an agent is mid-run when shutdown is requested, the current agent run completes before the loop exits.


## npm Distribution

troupe is distributed as a Go binary wrapped in npm packages, following the [esbuild pattern](https://esbuild.github.io/getting-started/#install-esbuild). This allows `npx @8ftdev/troupe --workflow-file WORKFLOW.MD` to work without a Go toolchain.

```
npm/
├── troupe/                        # Main package: @8ftdev/troupe
│   ├── package.json               #   optionalDependencies → platform packages
│   └── bin/troupe                 #   Node.js wrapper script
├── darwin-arm64/package.json      # @8ftdev/troupe-darwin-arm64 (os: darwin, cpu: arm64)
├── darwin-x64/package.json        # @8ftdev/troupe-darwin-x64   (os: darwin, cpu: x64)
├── linux-x64/package.json         # @8ftdev/troupe-linux-x64    (os: linux,  cpu: x64)
└── linux-arm64/package.json       # @8ftdev/troupe-linux-arm64  (os: linux,  cpu: arm64)
```

The main package lists all four platform packages as `optionalDependencies`. When npm installs, it only downloads the one matching the current OS/CPU — the `os` and `cpu` fields in each platform package's `package.json` control this.

The `bin/troupe` wrapper is a Node.js script that:
1. Detects `os.platform()` and `os.arch()`
2. Resolves the matching platform package via `require.resolve`
3. Execs the Go binary with `execFileSync`, inheriting stdio and exit code

`scripts/npm-pack.sh <version>` automates the build: cross-compiles Go binaries via `make dist`, copies each into the matching `npm/<platform>/bin/troupe`, and stamps the version in all `package.json` files.


## Cross-Cutting Concerns

### Configuration as a Single File

The WORKFLOW.MD file is intentionally a single file that contains both machine configuration (YAML) and the human-authored prompt (Go template). This means you can version a complete troupe setup — including the exact prompt an agent receives — in a single committed file. Changing the prompt is a PR, not a config change.

### External ID Linking

The `external_source` + `external_id` pair on Plane work items is the primary mechanism for bi-directional linking between GitHub and Plane. The sync layer writes this link when creating cards. The orchestrator reads it when matching cards to issues. This is a convention, not an API feature — Plane stores these as opaque string fields.

The external_id is the GitHub issue number as a string (e.g., `"42"`), not a URL or compound key. This keeps matching simple: `strconv.Atoi` and a linear scan.

### Progress Streaming

Agent progress flows through three layers:
1. The Claude SDK emits `AssistantMessage` blocks as the agent works
2. The runner parses these into human-readable strings (task lists, status text, PR URLs)
3. The orchestrator's `ProgressFunc` callback posts these as HTML comments on the Plane card

This gives humans watching the Kanban board a real-time view of what the agent is doing, without leaving Plane.

### Error Handling

troupe distinguishes between **fatal errors** (which abort startup) and **per-card errors** (which fail individual cards but keep the loop running).

Fatal: missing env vars, invalid workflow file, port binding failure, ngrok failure.
Per-card: GitHub issue not found, prompt render failure, agent error, agent timeout (max turns/budget).

Per-card errors always result in the card moving to "Failed" with an error comment. The orchestrator continues to the next card. This means a broken issue doesn't block the queue.

The sync layer is similarly resilient: if creating a single Plane card fails, it logs the error and continues to the next issue.

### Testing

There are 61 tests across 5 packages. The testing strategy varies by layer:

- **config** (7 tests): pure data transformation. Tests parse sample WORKFLOW.MD content and verify struct fields. No mocks needed.
- **github** (9 tests): HTTP-level. Tests start `httptest.Server` instances that return canned JSON, then verify the client parses responses correctly. Webhook tests construct HTTP requests and check handler behavior.
- **plane** (11 tests): same HTTP-level strategy as github. Tests verify request construction, response parsing, and error handling.
- **runner** (16 tests): the `session` interface is mockable. Tests inject a fake session that yields predetermined messages, then verify the runner produces the correct `Result`. Hook tests use real shell commands (`echo`).
- **orchestrator** (18 tests): all three dependencies (PlaneClient, GitHubClient, AgentRunner) are interfaces with in-test mock implementations. Tests cover the complete state machine: no items, success, failure, errors at each stage, oldest-first ordering, progress callbacks, shutdown.

`cmd/troupe` has no unit tests — it is integration-level glue code. The internal packages are where the logic lives and where the tests are.

**Architecture Invariant:** no test makes real API calls or starts real subprocesses (except runner hook tests which call `sh -c echo`). All network I/O is mocked via `httptest` or interface mocks. Tests are fast, deterministic, and run offline.

**Architecture Invariant:** the orchestrator's interfaces exist solely for testability. In production, the concrete `plane.Client` and `github.Client` implement them. There is no plugin system or runtime polymorphism beyond this.

### Logging

troupe uses `log/slog` (Go's structured logging stdlib) throughout. The logger is created once in `cmd/troupe` and passed to the orchestrator. Log output goes to stderr. The orchestrator logs at these levels:
- **Info**: card processing start/completion, state transitions, agent results
- **Debug**: "no triggered work items found" (the idle case)
- **Error**: per-card failures that don't crash the loop

### Graceful Shutdown

SIGINT and SIGTERM are caught via `signal.NotifyContext`, which cancels the root context. The orchestrator checks `ctx.Done()` at two points: at the top of the main loop (before processing) and during the poll-interval sleep (interruptible via `select`). If an agent is mid-run, the cancelled context propagates to the Claude subprocess, which terminates. The deferred cleanup in `cmd/troupe` then stops the HTTP server and ngrok.
