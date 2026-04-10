package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"time"
)

// Retry constants
const (
	maxRetries     = 5
	initialBackoff = 5 * time.Second
	maxBackoff     = 300 * time.Second
	backoffFactor  = 2.0
	backoffJitter  = 0.2 // 20% jitter
)

// Asset represents a release asset
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// Release represents a GitHub release
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	Prerelease  bool      `json:"prerelease"`
	Assets      []Asset   `json:"assets"`
}

// GitHubAPI handles fetching release data from a release index
type GitHubAPI struct {
	ctx     context.Context
	baseURL string
	channel string
	client  *http.Client
	logger  Logger
}

// Logger interface for logging
type Logger interface {
	Printf(format string, v ...any)
}

// NewGitHubAPI creates a new release index client
func NewGitHubAPI(ctx context.Context, baseURL, channel string, logger Logger) *GitHubAPI {
	return &GitHubAPI{
		ctx:     ctx,
		baseURL: baseURL,
		channel: channel,
		client:  &http.Client{Timeout: 10 * time.Second},
		logger:  logger,
	}
}

// GetReleases fetches releases from the GitHub API with exponential backoff retries
func (g *GitHubAPI) GetReleases() ([]Release, error) {
	var (
		resp      *http.Response
		err       error
		backoff   = initialBackoff
		retries   = 0
		releases  []Release
		lastError error
	)

	// Create the request outside the retry loop
	url := g.baseURL + "/" + g.channel + ".json"

	req, err := http.NewRequestWithContext(g.ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "librescoot-update-service")

	// Retry loop with exponential backoff
	for retries <= maxRetries {
		// Check if context is canceled before making the request
		if g.ctx.Err() != nil {
			return nil, fmt.Errorf("context canceled: %w", g.ctx.Err())
		}

		// Make the request
		resp, err = g.client.Do(req)

		// If request succeeded
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()

			// Decode the response
			if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
				return nil, fmt.Errorf("failed to decode response: %w", err)
			}

			// If we retried, log success after retries
			if retries > 0 {
				g.logger.Printf("Successfully fetched releases after %d retries", retries)
			}

			return releases, nil
		}

		// Handle response cleanup if we got a response but will retry
		if resp != nil {
			resp.Body.Close()
		}

		// Save the last error
		if err != nil {
			lastError = fmt.Errorf("failed to fetch releases: %w", err)
		} else {
			lastError = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		// If we've reached max retries, break out of the loop
		if retries == maxRetries {
			break
		}

		// Calculate jitter (between -jitter% and +jitter%)
		jitter := 1.0 + (rand.Float64()*2-1.0)*backoffJitter

		// Apply jitter to backoff
		actualBackoff := time.Duration(float64(backoff) * jitter)

		// Log retry attempt
		g.logger.Printf("Failed to fetch releases (attempt %d/%d), retrying in %v: %v",
			retries+1, maxRetries, actualBackoff, lastError)

		// Wait before retrying
		select {
		case <-time.After(actualBackoff):
			// Continue with retry
		case <-g.ctx.Done():
			return nil, fmt.Errorf("context canceled during backoff: %w", g.ctx.Err())
		}

		// Increase backoff for next attempt (with cap)
		backoff = min(time.Duration(float64(backoff)*backoffFactor), maxBackoff)

		retries++
	}

	// If we got here, we've exhausted all retries
	return nil, fmt.Errorf("failed to fetch releases after %d retries: %w", maxRetries, lastError)
}
