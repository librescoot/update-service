package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Asset represents a GitHub release asset
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Release represents a GitHub release
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// GitHubAPI handles interactions with the GitHub API
type GitHubAPI struct {
	ctx     context.Context
	baseURL string
	client  *http.Client
}

// NewGitHubAPI creates a new GitHub API client
func NewGitHubAPI(ctx context.Context, baseURL string) *GitHubAPI {
	return &GitHubAPI{
		ctx:     ctx,
		baseURL: baseURL,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// GetReleases fetches releases from the GitHub API
func (g *GitHubAPI) GetReleases() ([]Release, error) {
	req, err := http.NewRequestWithContext(g.ctx, "GET", g.baseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "librescoot-update-service")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var releases []Release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return releases, nil
}
