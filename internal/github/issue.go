// Package github provides a REST client for fetching GitHub issues and an HTTP
// handler for receiving GitHub webhook events.
package github

import "time"

// Issue is the internal representation of a GitHub issue used throughout troupe.
type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string // "open", "closed"
	Labels    []string
	CreatedAt time.Time
	HTMLURL   string
}

// Event represents a normalised GitHub webhook event relevant to troupe.
type Event struct {
	Action string // "opened", "edited", "closed", "labeled"
	Issue  Issue
}
