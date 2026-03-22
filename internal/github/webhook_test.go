package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookHandler_IssueOpened(t *testing.T) {
	handler, events := WebhookHandler("")

	body := `{
		"action": "opened",
		"issue": {
			"number": 42,
			"title": "Fix the button",
			"body": "The button is broken",
			"state": "open",
			"labels": [{"name": "bug"}],
			"html_url": "https://github.com/owner/repo/issues/42"
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case evt := <-events:
		if evt.Action != "opened" {
			t.Errorf("Action = %q, want %q", evt.Action, "opened")
		}
		if evt.Issue.Number != 42 {
			t.Errorf("Issue.Number = %d, want 42", evt.Issue.Number)
		}
		if evt.Issue.Title != "Fix the button" {
			t.Errorf("Issue.Title = %q, want %q", evt.Issue.Title, "Fix the button")
		}
		if len(evt.Issue.Labels) != 1 || evt.Issue.Labels[0] != "bug" {
			t.Errorf("Issue.Labels = %v, want [bug]", evt.Issue.Labels)
		}
	default:
		t.Fatal("expected event on channel, got none")
	}
}

func TestWebhookHandler_IgnoresNonIssueEvents(t *testing.T) {
	handler, events := WebhookHandler("")

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Event", "push")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case evt := <-events:
		t.Fatalf("unexpected event: %+v", evt)
	default:
	}
}

func TestWebhookHandler_IgnoresUnknownActions(t *testing.T) {
	handler, events := WebhookHandler("")

	body := `{"action": "transferred", "issue": {"number": 1}}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case evt := <-events:
		t.Fatalf("unexpected event for unknown action: %+v", evt)
	default:
	}
}

func TestWebhookHandler_RejectsGetMethod(t *testing.T) {
	handler, _ := WebhookHandler("")

	req := httptest.NewRequest(http.MethodGet, "/webhooks/github", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestWebhookHandler_SignatureValidation(t *testing.T) {
	secret := "test-secret"
	handler, events := WebhookHandler(secret)

	body := `{"action": "opened", "issue": {"number": 1, "title": "test", "state": "open"}}`

	// Valid signature.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid sig: status = %d, want %d", w.Code, http.StatusOK)
	}
	<-events // drain

	// Invalid signature.
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid sig: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	// Missing signature when secret is set.
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	w = httptest.NewRecorder()

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}
