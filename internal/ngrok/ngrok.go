// Package ngrok manages an ngrok tunnel subprocess to expose a local HTTP
// server to the internet for receiving GitHub webhooks.
package ngrok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"time"
)

// Tunnel represents a running ngrok tunnel.
type Tunnel struct {
	PublicURL string
	cmd       *exec.Cmd
}

// Start launches an ngrok tunnel forwarding to the given local port.
// It waits for ngrok to report a public URL via its local API.
func Start(ctx context.Context, port int) (*Tunnel, error) {
	cmd := exec.CommandContext(ctx, "ngrok", "http", fmt.Sprintf("%d", port), "--log=stderr")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ngrok: %w", err)
	}

	url, err := waitForURL(ctx)
	if err != nil {
		// Kill ngrok if we can't get the URL.
		_ = cmd.Process.Kill()
		return nil, err
	}

	return &Tunnel{PublicURL: url, cmd: cmd}, nil
}

// Stop terminates the ngrok process.
func (t *Tunnel) Stop() error {
	if t.cmd != nil && t.cmd.Process != nil {
		return t.cmd.Process.Kill()
	}
	return nil
}

// waitForURL polls the ngrok local API until a tunnel URL is available.
func waitForURL(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.After(15 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("timed out waiting for ngrok tunnel URL")
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:4040/api/tunnels", nil)
		if err != nil {
			return "", err
		}

		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		var result struct {
			Tunnels []struct {
				PublicURL string `json:"public_url"`
				Proto     string `json:"proto"`
			} `json:"tunnels"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			_ = resp.Body.Close()
			time.Sleep(500 * time.Millisecond)
			continue
		}
		_ = resp.Body.Close()

		for _, t := range result.Tunnels {
			if t.Proto == "https" {
				return t.PublicURL, nil
			}
		}

		time.Sleep(500 * time.Millisecond)
	}
}
