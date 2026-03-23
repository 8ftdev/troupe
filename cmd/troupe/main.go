// Package main is the entry point for the troupe CLI.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/8ftdev/troupe/internal/config"
	gh "github.com/8ftdev/troupe/internal/github"
	"github.com/8ftdev/troupe/internal/ngrok"
	"github.com/8ftdev/troupe/internal/orchestrator"
	"github.com/8ftdev/troupe/internal/plane"
	"github.com/8ftdev/troupe/internal/runner"
)

var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		workflowFile string
		pollInterval time.Duration
		webhookPort  int
	)

	cmd := &cobra.Command{
		Use:     "troupe",
		Short:   "Claude Code agent orchestrator",
		Long:    "troupe syncs GitHub Issues to a Plane Kanban board and spawns Claude Code agents to work on them.",
		Version: version,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), workflowFile, pollInterval, webhookPort)
		},
		SilenceUsage: true,
	}

	cmd.Flags().StringVarP(&workflowFile, "workflow-file", "f", "", "path to WORKFLOW.MD file (required)")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 30*time.Second, "how often to poll Plane for triggered cards")
	cmd.Flags().IntVar(&webhookPort, "webhook-port", 8090, "local port for GitHub webhook listener")
	_ = cmd.MarkFlagRequired("workflow-file")

	return cmd
}

func run(ctx context.Context, workflowFile string, pollInterval time.Duration, webhookPort int) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// --- Load config ---
	cfg, err := config.Load(workflowFile)
	if err != nil {
		return fmt.Errorf("loading workflow file: %w", err)
	}
	log.Info("config loaded", "name", cfg.Name, "repo", cfg.Repo)

	// --- Validate required env vars ---
	githubToken := os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		return fmt.Errorf("GITHUB_TOKEN environment variable is required")
	}
	if cfg.Plane.APIKey == "" {
		return fmt.Errorf("plane API key is required (set PLANE_API_KEY and use ${PLANE_API_KEY} in workflow file)")
	}

	// --- Create clients ---
	ghClient, err := gh.NewClient(githubToken, cfg.Repo)
	if err != nil {
		return fmt.Errorf("creating GitHub client: %w", err)
	}

	planeClient := plane.NewClient(cfg.Plane.APIKey, cfg.Plane.BaseURL, cfg.Plane.Workspace, cfg.Plane.Project)

	// --- Signal handling ---
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Initial sync: GitHub issues → Plane cards ---
	log.Info("syncing GitHub issues to Plane")
	if err := syncIssuesToPlane(ctx, ghClient, planeClient, cfg, log); err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// --- Start webhook listener ---
	webhookSecret := os.Getenv("WEBHOOK_SECRET")
	handler, events := gh.WebhookHandler(webhookSecret)

	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", webhookPort))
	if err != nil {
		return fmt.Errorf("binding webhook port %d: %w", webhookPort, err)
	}
	srv := &http.Server{Handler: mux}

	go func() {
		log.Info("webhook listener started", "port", webhookPort)
		if srvErr := srv.Serve(listener); srvErr != nil && srvErr != http.ErrServerClosed {
			log.Error("webhook server error", "error", srvErr)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// --- Start ngrok tunnel ---
	log.Info("starting ngrok tunnel", "port", webhookPort)
	tunnel, err := ngrok.Start(ctx, webhookPort)
	if err != nil {
		return fmt.Errorf("starting ngrok: %w", err)
	}
	defer func() { _ = tunnel.Stop() }()
	log.Info("ngrok tunnel established", "url", tunnel.PublicURL)

	// --- Register GitHub webhook ---
	webhookURL := tunnel.PublicURL + "/webhooks/github"
	log.Info("registering GitHub webhook", "url", webhookURL)
	if err := ghClient.RegisterWebhook(ctx, webhookURL, webhookSecret); err != nil {
		log.Warn("webhook registration failed (may already exist)", "error", err)
	}

	// --- Background: webhook events → Plane sync ---
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-events:
				handleWebhookEvent(ctx, event, planeClient, cfg, log)
			}
		}
	}()

	// --- Determine working directory ---
	repoDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// --- Create runner factory ---
	newRunner := func(onProgress runner.ProgressFunc) orchestrator.AgentRunner {
		return runner.New(cfg, repoDir, onProgress)
	}

	// --- Start orchestrator ---
	orch := orchestrator.New(cfg, planeClient, ghClient, repoDir, pollInterval, log, newRunner)
	log.Info("orchestrator starting")
	return orch.Run(ctx)
}

// syncIssuesToPlane fetches all matching GitHub issues and creates corresponding
// Plane work items for any that don't already exist.
func syncIssuesToPlane(ctx context.Context, ghClient *gh.Client, planeClient *plane.Client, cfg *config.Config, log *slog.Logger) error {
	issues, err := ghClient.FetchIssues(ctx, cfg.GitHub.Labels)
	if err != nil {
		return fmt.Errorf("fetching GitHub issues: %w", err)
	}
	log.Info("fetched GitHub issues", "count", len(issues))

	for _, issue := range issues {
		externalID := strconv.Itoa(issue.Number)
		existing, err := planeClient.FindWorkItemByExternalID(ctx, "github", externalID)
		if err != nil {
			log.Error("checking existing work item", "issue", issue.Number, "error", err)
			continue
		}
		if existing != nil {
			log.Debug("work item already exists", "issue", issue.Number, "card_id", existing.ID)
			continue
		}

		name := fmt.Sprintf("#%d %s", issue.Number, issue.Title)
		description := formatIssueDescription(issue)

		_, err = planeClient.CreateWorkItem(ctx, &plane.CreateWorkItemRequest{
			Name:           name,
			Description:    description,
			ExternalSource: "github",
			ExternalID:     externalID,
		})
		if err != nil {
			log.Error("creating work item", "issue", issue.Number, "error", err)
			continue
		}
		log.Info("created work item", "issue", issue.Number, "title", issue.Title)
	}

	return nil
}

// handleWebhookEvent processes a single GitHub webhook event by syncing
// the issue to Plane.
func handleWebhookEvent(ctx context.Context, event gh.Event, planeClient *plane.Client, cfg *config.Config, log *slog.Logger) {
	log.Info("webhook event received", "action", event.Action, "issue", event.Issue.Number)

	externalID := strconv.Itoa(event.Issue.Number)
	existing, err := planeClient.FindWorkItemByExternalID(ctx, "github", externalID)
	if err != nil {
		log.Error("checking existing work item", "issue", event.Issue.Number, "error", err)
		return
	}

	switch event.Action {
	case "opened":
		if existing != nil {
			return
		}
		name := fmt.Sprintf("#%d %s", event.Issue.Number, event.Issue.Title)
		description := formatIssueDescription(event.Issue)
		_, err = planeClient.CreateWorkItem(ctx, &plane.CreateWorkItemRequest{
			Name:           name,
			Description:    description,
			ExternalSource: "github",
			ExternalID:     externalID,
		})
		if err != nil {
			log.Error("creating work item from webhook", "issue", event.Issue.Number, "error", err)
		}

	case "edited":
		if existing == nil {
			return
		}
		name := fmt.Sprintf("#%d %s", event.Issue.Number, event.Issue.Title)
		description := formatIssueDescription(event.Issue)
		_, err = planeClient.UpdateWorkItem(ctx, existing.ID, &plane.UpdateWorkItemRequest{
			Name:        name,
			Description: description,
		})
		if err != nil {
			log.Error("updating work item from webhook", "issue", event.Issue.Number, "error", err)
		}

	case "closed":
		if existing == nil {
			return
		}
		stateID, err := planeClient.ResolveStateID(ctx, cfg.Plane.DoneState)
		if err != nil {
			log.Error("resolving done state", "error", err)
			return
		}
		_, err = planeClient.UpdateWorkItem(ctx, existing.ID, &plane.UpdateWorkItemRequest{
			StateID: stateID,
		})
		if err != nil {
			log.Error("closing work item from webhook", "issue", event.Issue.Number, "error", err)
		}

	case "labeled":
		if existing != nil {
			return
		}
		// Only create if the issue has matching labels.
		if !hasMatchingLabels(event.Issue.Labels, cfg.GitHub.Labels) {
			return
		}
		name := fmt.Sprintf("#%d %s", event.Issue.Number, event.Issue.Title)
		description := formatIssueDescription(event.Issue)
		_, err = planeClient.CreateWorkItem(ctx, &plane.CreateWorkItemRequest{
			Name:           name,
			Description:    description,
			ExternalSource: "github",
			ExternalID:     externalID,
		})
		if err != nil {
			log.Error("creating work item from label event", "issue", event.Issue.Number, "error", err)
		}
	}
}

// hasMatchingLabels returns true if any of the issue's labels match the filter.
func hasMatchingLabels(issueLabels, filterLabels []string) bool {
	if len(filterLabels) == 0 {
		return true
	}
	for _, fl := range filterLabels {
		for _, il := range issueLabels {
			if strings.EqualFold(il, fl) {
				return true
			}
		}
	}
	return false
}

// formatIssueDescription wraps issue body text in basic HTML for Plane.
func formatIssueDescription(issue gh.Issue) string {
	return fmt.Sprintf("<p><a href=%q>GitHub Issue #%d</a></p><p>%s</p>", issue.HTMLURL, issue.Number, issue.Body)
}
