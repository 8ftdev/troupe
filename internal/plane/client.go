package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client communicates with the Plane REST API for a single project.
type Client struct {
	apiKey    string
	baseURL   string // e.g. "http://localhost:8080"
	workspace string
	projectID string
	http      *http.Client

	// Cached state map (name → ID), populated on first ResolveStateID call.
	states map[string]string
}

// NewClient creates a Plane API client for the given workspace and project.
func NewClient(apiKey, baseURL, workspace, projectID string) *Client {
	return &Client{
		apiKey:    apiKey,
		baseURL:   strings.TrimRight(baseURL, "/"),
		workspace: workspace,
		projectID: projectID,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

// ListStates returns all states for the project.
func (c *Client) ListStates(ctx context.Context) ([]State, error) {
	url := c.projectURL() + "/states/"

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching states: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}

	var page paginatedResponse[State]
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decoding states: %w", err)
	}
	return page.Results, nil
}

// ResolveStateID maps a state name to its Plane UUID.
// Results are cached after the first call.
func (c *Client) ResolveStateID(ctx context.Context, name string) (string, error) {
	if c.states == nil {
		states, err := c.ListStates(ctx)
		if err != nil {
			return "", err
		}
		c.states = make(map[string]string, len(states))
		for _, s := range states {
			c.states[s.Name] = s.ID
		}
	}
	id, ok := c.states[name]
	if !ok {
		return "", fmt.Errorf("state %q not found in project", name)
	}
	return id, nil
}

// ListWorkItems returns work items, optionally filtered by state ID.
// Pass an empty stateID to list all work items.
func (c *Client) ListWorkItems(ctx context.Context, stateID string) ([]WorkItem, error) {
	url := c.projectURL() + "/work-items/"
	if stateID != "" {
		url += "?state=" + stateID
	}

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching work items: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}

	var page paginatedResponse[WorkItem]
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decoding work items: %w", err)
	}
	return page.Results, nil
}

// CreateWorkItem creates a new work item in Plane.
func (c *Client) CreateWorkItem(ctx context.Context, item *CreateWorkItemRequest) (*WorkItem, error) {
	url := c.projectURL() + "/work-items/"

	body, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("creating work item: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return nil, apiError(resp)
	}

	var created WorkItem
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &created, nil
}

// UpdateWorkItem patches an existing work item.
func (c *Client) UpdateWorkItem(ctx context.Context, workItemID string, update *UpdateWorkItemRequest) (*WorkItem, error) {
	url := fmt.Sprintf("%s/work-items/%s/", c.projectURL(), workItemID)

	body, err := json.Marshal(update)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPatch, url, body)
	if err != nil {
		return nil, fmt.Errorf("updating work item: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}

	var updated WorkItem
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &updated, nil
}

// CreateComment posts a comment on a work item.
func (c *Client) CreateComment(ctx context.Context, workItemID, commentHTML string) (*Comment, error) {
	url := fmt.Sprintf("%s/work-items/%s/comments/", c.projectURL(), workItemID)

	payload := struct {
		CommentHTML string `json:"comment_html"`
	}{CommentHTML: commentHTML}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	resp, err := c.doRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("creating comment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return nil, apiError(resp)
	}

	var created Comment
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &created, nil
}

// FindWorkItemByExternalID returns the work item matching the given external
// source and ID, or nil if not found.
func (c *Client) FindWorkItemByExternalID(ctx context.Context, source, externalID string) (*WorkItem, error) {
	items, err := c.ListWorkItems(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ExternalSource == source && item.ExternalID == externalID {
			return &item, nil
		}
	}
	return nil, nil
}

// projectURL returns the base URL for project-scoped API calls.
func (c *Client) projectURL() string {
	return fmt.Sprintf("%s/api/v1/workspaces/%s/projects/%s",
		c.baseURL, c.workspace, c.projectID)
}

// doRequest creates and executes an authenticated HTTP request.
func (c *Client) doRequest(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	return c.http.Do(req)
}

// paginatedResponse is the envelope for Plane paginated API responses.
type paginatedResponse[T any] struct {
	Results []T `json:"results"`
}

// apiError reads the response body and returns a formatted error.
func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("plane API returned %d: %s", resp.StatusCode, body)
}
