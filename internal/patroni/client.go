// Package patroni is a small HTTP client for the Patroni REST API.
package patroni

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Member is a single Patroni cluster member as returned by GET /cluster.
type Member struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	State    string `json:"state"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Lag      *int64 `json:"lag"`
	Timeline *int64 `json:"timeline"`
}

// Cluster is the payload returned by GET /cluster.
type Cluster struct {
	Members []Member `json:"members"`
}

// Leader returns the name of the current leader, if any.
func (c *Cluster) Leader() (string, bool) {
	for _, m := range c.Members {
		if m.Role == "leader" || m.Role == "primary" {
			return m.Name, true
		}
	}
	return "", false
}

// Client talks to one Patroni instance.
type Client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

// New creates a Patroni API client. An empty timeout means 3 seconds.
func New(baseURL, username, password string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Client{
		baseURL:  baseURL,
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// Cluster fetches the current cluster state.
func (c *Client) Cluster(ctx context.Context) (*Cluster, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/cluster", nil)
	if err != nil {
		return nil, fmt.Errorf("build patroni request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call patroni /cluster: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("patroni /cluster returned status %d", resp.StatusCode)
	}

	var cluster Cluster
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cluster); err != nil {
		return nil, fmt.Errorf("decode patroni response: %w", err)
	}
	return &cluster, nil
}
