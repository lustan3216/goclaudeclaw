// Package memory wraps claude-mem / mem0 REST API calls,
// providing a unified shared memory read/write interface for all bots.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Memory is a single memory entry.
type Memory struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Score   float64 `json:"score,omitempty"` // search relevance score
}

// SearchResult is the response from the search endpoint.
type SearchResult struct {
	Memories []Memory `json:"memories"`
}

// Client is an HTTP client for communicating with the claude-mem service.
type Client struct {
	endpoint   string
	httpClient *http.Client
}

// New creates a new Client.
// Example endpoint: "http://localhost:8080"
func New(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Search searches for memories relevant to the query text, returning up to topK results.
func (c *Client) Search(ctx context.Context, query string, topK int) ([]Memory, error) {
	if topK <= 0 {
		topK = 5
	}
	payload := map[string]any{
		"query": query,
		"top_k": topK,
	}
	var result SearchResult
	if err := c.post(ctx, "/search", payload, &result); err != nil {
		return nil, fmt.Errorf("memory search failed: %w", err)
	}
	slog.Debug("memory search complete", "query", query, "results", len(result.Memories))
	return result.Memories, nil
}

// Add adds a new memory entry to claude-mem.
// content is natural language text; metadata is optional additional fields (may be nil).
func (c *Client) Add(ctx context.Context, content string, metadata map[string]any) error {
	payload := map[string]any{
		"content":  content,
		"metadata": metadata,
	}
	if err := c.post(ctx, "/add", payload, nil); err != nil {
		return fmt.Errorf("failed to add memory: %w", err)
	}
	slog.Debug("memory added", "content_len", len(content))
	return nil
}

// Delete deletes a memory entry by ID.
func (c *Client) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.endpoint+"/memories/"+id, nil)
	if err != nil {
		return fmt.Errorf("failed to build delete request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to delete memory HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// Health checks whether the claude-mem service is available.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("claude-mem service unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("claude-mem health check failed HTTP %d", resp.StatusCode)
	}
	return nil
}

// post sends a JSON POST request and decodes the response JSON into out (ignored if out is nil).
func (c *Client) post(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to serialize request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("failed to parse response JSON: %w", err)
		}
	}
	return nil
}
