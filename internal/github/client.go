package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Client fetches issues from the GitHub REST API.
type Client struct {
	token   string
	owner   string
	repo    string
	baseURL string // override for testing; defaults to https://api.github.com
	http    *http.Client
}

// NewClient creates a GitHub client for the given owner/repo.
// token is a GitHub personal access token or empty for public repos.
func NewClient(token, ownerRepo string) (*Client, error) {
	parts := strings.SplitN(ownerRepo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid repo format %q, expected owner/repo", ownerRepo)
	}
	return &Client{
		token:   token,
		owner:   parts[0],
		repo:    parts[1],
		baseURL: "https://api.github.com",
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// apiIssue is the JSON shape returned by the GitHub REST API.
type apiIssue struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	HTMLURL   string     `json:"html_url"`
	Labels    []apiLabel `json:"labels"`
	// Pull requests also appear in the issues endpoint; filter them out.
	PullRequest *struct{} `json:"pull_request,omitempty"`
}

type apiLabel struct {
	Name string `json:"name"`
}

// FetchIssues returns open issues matching the given labels, sorted by
// created_at ascending (oldest first). It paginates automatically.
func (c *Client) FetchIssues(ctx context.Context, labels []string) ([]Issue, error) {
	var all []Issue
	page := 1

	for {
		url := fmt.Sprintf("%s/repos/%s/%s/issues?state=open&sort=created&direction=asc&per_page=100&page=%d",
			c.baseURL, c.owner, c.repo, page)
		if len(labels) > 0 {
			url += "&labels=" + strings.Join(labels, ",")
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		c.setAuthHeader(req)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching issues page %d: %w", page, err)
		}

		issues, err := c.decodeIssuesPage(resp)
		if err != nil {
			return nil, fmt.Errorf("decoding issues page %d: %w", page, err)
		}

		if len(issues) == 0 {
			break
		}
		all = append(all, issues...)
		page++
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].CreatedAt.Before(all[j].CreatedAt)
	})

	return all, nil
}

func (c *Client) decodeIssuesPage(resp *http.Response) ([]Issue, error) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, body)
	}

	var raw []apiIssue
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding JSON: %w", err)
	}

	var issues []Issue
	for _, r := range raw {
		// Skip pull requests (they also appear in the issues endpoint).
		if r.PullRequest != nil {
			continue
		}
		labels := make([]string, len(r.Labels))
		for i, l := range r.Labels {
			labels[i] = l.Name
		}
		issues = append(issues, Issue{
			Number:    r.Number,
			Title:     r.Title,
			Body:      r.Body,
			State:     r.State,
			Labels:    labels,
			CreatedAt: r.CreatedAt,
			HTMLURL:   r.HTMLURL,
		})
	}
	return issues, nil
}

func (c *Client) setAuthHeader(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
