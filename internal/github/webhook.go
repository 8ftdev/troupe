package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// WebhookHandler returns an http.Handler that processes GitHub webhook events
// for issues. Validated events are sent to the returned channel.
// If webhookSecret is non-empty, payloads are verified using HMAC-SHA256.
func WebhookHandler(webhookSecret string) (http.Handler, <-chan Event) {
	ch := make(chan Event, 64)
	h := &webhookHandler{
		secret: webhookSecret,
		events: ch,
	}
	return h, ch
}

type webhookHandler struct {
	secret string
	events chan<- Event
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	if eventType != "issues" {
		// Ignore non-issue events (ping, push, etc.)
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer func() { _ = r.Body.Close() }()

	if h.secret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !verifySignature(h.secret, sig, body) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	event, ok := payload.toEvent()
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Non-blocking send; drop event if channel is full.
	select {
	case h.events <- event:
	default:
	}

	w.WriteHeader(http.StatusOK)
}

type webhookPayload struct {
	Action string   `json:"action"`
	Issue  apiIssue `json:"issue"`
}

func (p *webhookPayload) toEvent() (Event, bool) {
	switch p.Action {
	case "opened", "edited", "closed", "labeled":
	default:
		return Event{}, false
	}

	labels := make([]string, len(p.Issue.Labels))
	for i, l := range p.Issue.Labels {
		labels[i] = l.Name
	}

	return Event{
		Action: p.Action,
		Issue: Issue{
			Number:    p.Issue.Number,
			Title:     p.Issue.Title,
			Body:      p.Issue.Body,
			State:     p.Issue.State,
			Labels:    labels,
			CreatedAt: p.Issue.CreatedAt,
			HTMLURL:   p.Issue.HTMLURL,
		},
	}, true
}

func verifySignature(secret, signature string, body []byte) bool {
	if signature == "" {
		return false
	}
	const prefix = "sha256="
	if len(signature) <= len(prefix) {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := prefix + hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}

// RegisterWebhook creates a webhook on the GitHub repo pointing to the given URL.
// It is used to register the ngrok tunnel URL after startup.
func (c *Client) RegisterWebhook(ctx context.Context, webhookURL, secret string) error {
	payload := map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"issues"},
		"config": map[string]string{
			"url":          webhookURL,
			"content_type": "json",
			"secret":       secret,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling webhook payload: %w", err)
	}

	reqURL := fmt.Sprintf("%s/repos/%s/%s/hooks", c.baseURL, c.owner, c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setAuthHeader(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("registering webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}
