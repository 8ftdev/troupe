// Package orchestrator implements the sequential issue processing loop.
// It polls Plane for triggered work items, matches them to GitHub issues,
// spawns a Claude Code agent via the runner, and updates card state.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/8ftdev/troupe/internal/config"
	gh "github.com/8ftdev/troupe/internal/github"
	"github.com/8ftdev/troupe/internal/plane"
	"github.com/8ftdev/troupe/internal/runner"
)

// PlaneClient abstracts the plane.Client methods used by the orchestrator.
type PlaneClient interface {
	ResolveStateID(ctx context.Context, name string) (string, error)
	ListWorkItems(ctx context.Context, stateID string) ([]plane.WorkItem, error)
	UpdateWorkItem(ctx context.Context, id string, req *plane.UpdateWorkItemRequest) (*plane.WorkItem, error)
	CreateComment(ctx context.Context, workItemID, html string) (*plane.Comment, error)
}

// GitHubClient abstracts the github.Client methods used by the orchestrator.
type GitHubClient interface {
	FetchIssues(ctx context.Context, labels []string) ([]gh.Issue, error)
}

// AgentRunner abstracts runner.Runner.Run for testability.
type AgentRunner interface {
	Run(ctx context.Context, prompt string) (*runner.Result, error)
}

// RunnerFactory creates an AgentRunner with the given progress callback.
// A new runner is created per work item so that progress updates are
// scoped to the correct Plane card.
type RunnerFactory func(onProgress runner.ProgressFunc) AgentRunner

// Orchestrator manages the sequential issue processing loop.
type Orchestrator struct {
	cfg          *config.Config
	plane        PlaneClient
	github       GitHubClient
	repoDir      string
	pollInterval time.Duration
	log          *slog.Logger
	newRunner    RunnerFactory
}

// New creates an Orchestrator. The pollInterval controls how often Plane is
// polled when no triggered work items are found.
func New(cfg *config.Config, p PlaneClient, g GitHubClient, repoDir string, pollInterval time.Duration, log *slog.Logger, newRunner RunnerFactory) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{
		cfg:          cfg,
		plane:        p,
		github:       g,
		repoDir:      repoDir,
		pollInterval: pollInterval,
		log:          log,
		newRunner:    newRunner,
	}
}

// Run starts the processing loop. It blocks until ctx is cancelled.
// On cancellation it finishes the current agent run before returning.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.log.Info("orchestrator started", "repo", o.cfg.Repo, "trigger_state", o.cfg.Plane.TriggerState, "poll_interval", o.pollInterval)

	for {
		select {
		case <-ctx.Done():
			o.log.Info("orchestrator shutting down")
			return ctx.Err()
		default:
		}

		processed, err := o.processNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				o.log.Info("orchestrator shutting down")
				return ctx.Err()
			}
			o.log.Error("processing error", "error", err)
		}

		if !processed {
			select {
			case <-ctx.Done():
				o.log.Info("orchestrator shutting down")
				return ctx.Err()
			case <-time.After(o.pollInterval):
			}
		}
	}
}

// processNext finds the next triggered work item, runs the agent, and updates
// the card. Returns true if a work item was processed.
func (o *Orchestrator) processNext(ctx context.Context) (bool, error) {
	triggerStateID, err := o.plane.ResolveStateID(ctx, o.cfg.Plane.TriggerState)
	if err != nil {
		return false, fmt.Errorf("resolving trigger state: %w", err)
	}

	items, err := o.plane.ListWorkItems(ctx, triggerStateID)
	if err != nil {
		return false, fmt.Errorf("listing triggered work items: %w", err)
	}

	card := pickOldest(items)
	if card == nil {
		o.log.Debug("no triggered work items found")
		return false, nil
	}

	o.log.Info("processing work item", "id", card.ID, "name", card.Name, "external_id", card.ExternalID)

	issue, err := o.matchGitHubIssue(ctx, card)
	if err != nil {
		o.log.Error("failed to match GitHub issue", "card_id", card.ID, "error", err)
		return true, o.failCard(ctx, card.ID, fmt.Sprintf("Failed to match GitHub issue: %v", err))
	}

	prompt, err := o.cfg.RenderPrompt(config.Issue{
		Number: issue.Number,
		Title:  issue.Title,
		Body:   issue.Body,
	})
	if err != nil {
		return true, o.failCard(ctx, card.ID, fmt.Sprintf("Failed to render prompt: %v", err))
	}

	_, _ = o.plane.CreateComment(ctx, card.ID, "<p>Agent starting...</p>")

	onProgress := func(update string) {
		_, _ = o.plane.CreateComment(ctx, card.ID, "<p>"+update+"</p>")
	}
	agentRunner := o.newRunner(onProgress)

	o.log.Info("running agent", "card_id", card.ID, "issue", issue.Number)
	result, err := agentRunner.Run(ctx, prompt)
	if err != nil {
		o.log.Error("agent run failed", "card_id", card.ID, "error", err)
		return true, o.failCard(ctx, card.ID, fmt.Sprintf("Agent error: %v", err))
	}

	return true, o.handleResult(ctx, card.ID, result)
}

// handleResult moves the card to Done or Failed based on the agent result.
func (o *Orchestrator) handleResult(ctx context.Context, cardID string, result *runner.Result) error {
	o.log.Info("agent completed",
		"card_id", cardID,
		"success", result.Success,
		"pr_created", result.PRCreated,
		"pr_url", result.PRURL,
		"turns", result.NumTurns,
		"duration", result.Duration,
		"cost_usd", result.CostUSD,
	)

	if result.Success && result.PRCreated {
		comment := fmt.Sprintf("<p>Agent completed. PR created: <a href=%q>%s</a></p>", result.PRURL, result.PRURL)
		_, _ = o.plane.CreateComment(ctx, cardID, comment)
		return o.moveCard(ctx, cardID, o.cfg.Plane.DoneState)
	}

	errMsg := result.Error
	if errMsg == "" {
		errMsg = "agent completed without creating a PR"
	}
	return o.failCard(ctx, cardID, errMsg)
}

// failCard posts an error comment and moves the card to the failed state.
func (o *Orchestrator) failCard(ctx context.Context, cardID, errMsg string) error {
	comment := fmt.Sprintf("<p>Agent failed: %s</p>", errMsg)
	_, _ = o.plane.CreateComment(ctx, cardID, comment)
	return o.moveCard(ctx, cardID, o.cfg.Plane.FailedState)
}

// moveCard transitions a work item to the named state.
func (o *Orchestrator) moveCard(ctx context.Context, cardID, stateName string) error {
	stateID, err := o.plane.ResolveStateID(ctx, stateName)
	if err != nil {
		return fmt.Errorf("resolving state %q: %w", stateName, err)
	}
	_, err = o.plane.UpdateWorkItem(ctx, cardID, &plane.UpdateWorkItemRequest{StateID: stateID})
	if err != nil {
		return fmt.Errorf("moving card to %q: %w", stateName, err)
	}
	o.log.Info("card moved", "card_id", cardID, "state", stateName)
	return nil
}

// matchGitHubIssue finds the GitHub issue that corresponds to a Plane work
// item by parsing the external_id (which stores the issue number as a string).
func (o *Orchestrator) matchGitHubIssue(ctx context.Context, card *plane.WorkItem) (*gh.Issue, error) {
	if card.ExternalSource != "github" || card.ExternalID == "" {
		return nil, fmt.Errorf("card %q has no GitHub external link", card.Name)
	}

	issueNum, err := strconv.Atoi(card.ExternalID)
	if err != nil {
		return nil, fmt.Errorf("invalid external_id %q: %w", card.ExternalID, err)
	}

	issues, err := o.github.FetchIssues(ctx, o.cfg.GitHub.Labels)
	if err != nil {
		return nil, fmt.Errorf("fetching GitHub issues: %w", err)
	}

	for i := range issues {
		if issues[i].Number == issueNum {
			return &issues[i], nil
		}
	}
	return nil, fmt.Errorf("GitHub issue #%d not found", issueNum)
}

// pickOldest returns the work item with the earliest CreatedAt, or nil if empty.
func pickOldest(items []plane.WorkItem) *plane.WorkItem {
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	return &items[0]
}
