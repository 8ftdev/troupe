// Package plane provides a REST client for the Plane project management API.
// It manages work items (Kanban cards), states, and comments.
package plane

import "time"

// State represents a Plane project state (e.g., "Todo", "In Progress", "Done").
type State struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"` // "backlog", "unstarted", "started", "completed", "cancelled"
	Color string `json:"color"`
}

// WorkItem represents a Plane work item (issue/card on the Kanban board).
type WorkItem struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description_html"`
	StateID        string    `json:"state"`
	Priority       string    `json:"priority"`
	SequenceID     int       `json:"sequence_id"`
	ExternalSource string    `json:"external_source"`
	ExternalID     string    `json:"external_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Comment represents a comment on a Plane work item.
type Comment struct {
	ID          string    `json:"id"`
	CommentHTML string    `json:"comment_html"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateWorkItemRequest is the request body for creating a work item.
type CreateWorkItemRequest struct {
	Name           string `json:"name"`
	Description    string `json:"description_html,omitempty"`
	StateID        string `json:"state,omitempty"`
	Priority       string `json:"priority,omitempty"`
	ExternalSource string `json:"external_source,omitempty"`
	ExternalID     string `json:"external_id,omitempty"`
}

// UpdateWorkItemRequest is the request body for updating a work item.
type UpdateWorkItemRequest struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description_html,omitempty"`
	StateID     string `json:"state,omitempty"`
	Priority    string `json:"priority,omitempty"`
}
