package status

import (
	"context"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

// Status represents the possible update states
type Status string

const (
	StatusIdle        Status = "idle"
	StatusDownloading Status = "downloading"
	StatusInstalling  Status = "installing"
	StatusRebooting   Status = "rebooting"
	StatusError       Status = "error"
)

// Reporter handles Redis status reporting for OTA updates
type Reporter struct {
	client    *redis.Client
	component string
	logger    *log.Logger
}

// NewReporter creates a new status reporter for the given component
func NewReporter(client *redis.Client, component string, logger *log.Logger) *Reporter {
	return &Reporter{
		client:    client,
		component: component,
		logger:    logger,
	}
}

// SetStatus updates the status for this component in Redis
func (r *Reporter) SetStatus(ctx context.Context, status Status) error {
	key := fmt.Sprintf("status:%s", r.component)

	err := r.client.HSet(ctx, "ota", key, string(status)).Err()
	if err != nil {
		return fmt.Errorf("failed to set status for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set status for %s: %s", r.component, status)
	return nil
}

// SetUpdateVersion updates the target update version for this component in Redis
func (r *Reporter) SetUpdateVersion(ctx context.Context, version string) error {
	key := fmt.Sprintf("update-version:%s", r.component)

	err := r.client.HSet(ctx, "ota", key, version).Err()
	if err != nil {
		return fmt.Errorf("failed to set update version for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set update version for %s: %s", r.component, version)
	return nil
}

// ClearUpdateVersion removes the update version for this component from Redis
func (r *Reporter) ClearUpdateVersion(ctx context.Context) error {
	key := fmt.Sprintf("update-version:%s", r.component)

	err := r.client.HDel(ctx, "ota", key).Err()
	if err != nil {
		return fmt.Errorf("failed to clear update version for component %s: %w", r.component, err)
	}

	r.logger.Printf("Cleared update version for %s", r.component)
	return nil
}

// GetStatus retrieves the current status for this component from Redis
func (r *Reporter) GetStatus(ctx context.Context) (Status, error) {
	key := fmt.Sprintf("status:%s", r.component)

	result, err := r.client.HGet(ctx, "ota", key).Result()
	if err == redis.Nil {
		return StatusIdle, nil // Default to idle if not set
	}
	if err != nil {
		return "", fmt.Errorf("failed to get status for component %s: %w", r.component, err)
	}

	return Status(result), nil
}

// SetStatusAndVersion atomically sets both status and update version
func (r *Reporter) SetStatusAndVersion(ctx context.Context, status Status, version string) error {
	pipe := r.client.Pipeline()

	statusKey := fmt.Sprintf("status:%s", r.component)
	versionKey := fmt.Sprintf("update-version:%s", r.component)

	pipe.HSet(ctx, "ota", statusKey, string(status))
	pipe.HSet(ctx, "ota", versionKey, version)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to set status and version for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set status and version for %s: %s, %s", r.component, status, version)
	return nil
}

// SetIdleAndClearVersion atomically sets status to idle and clears update version, error, progress, and method keys
func (r *Reporter) SetIdleAndClearVersion(ctx context.Context) error {
	pipe := r.client.Pipeline()

	statusKey := fmt.Sprintf("status:%s", r.component)
	versionKey := fmt.Sprintf("update-version:%s", r.component)
	errorKey := fmt.Sprintf("error:%s", r.component)
	errorMessageKey := fmt.Sprintf("error-message:%s", r.component)
	progressKey := fmt.Sprintf("download-progress:%s", r.component)
	downloadedKey := fmt.Sprintf("download-bytes:%s", r.component)
	totalKey := fmt.Sprintf("download-total:%s", r.component)
	methodKey := fmt.Sprintf("update-method:%s", r.component)

	pipe.HSet(ctx, "ota", statusKey, string(StatusIdle))
	pipe.HDel(ctx, "ota", versionKey)
	pipe.HDel(ctx, "ota", errorKey)
	pipe.HDel(ctx, "ota", errorMessageKey)
	pipe.HDel(ctx, "ota", progressKey)
	pipe.HDel(ctx, "ota", downloadedKey)
	pipe.HDel(ctx, "ota", totalKey)
	pipe.HDel(ctx, "ota", methodKey)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to set idle and clear version for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set status to idle and cleared version for %s", r.component)
	return nil
}

// SetError atomically sets status to error, stores error details, and clears download progress
func (r *Reporter) SetError(ctx context.Context, errorType, errorMessage string) error {
	pipe := r.client.Pipeline()

	statusKey := fmt.Sprintf("status:%s", r.component)
	errorKey := fmt.Sprintf("error:%s", r.component)
	errorMessageKey := fmt.Sprintf("error-message:%s", r.component)
	progressKey := fmt.Sprintf("download-progress:%s", r.component)
	downloadedKey := fmt.Sprintf("download-bytes:%s", r.component)
	totalKey := fmt.Sprintf("download-total:%s", r.component)

	pipe.HSet(ctx, "ota", statusKey, string(StatusError))
	pipe.HSet(ctx, "ota", errorKey, errorType)
	pipe.HSet(ctx, "ota", errorMessageKey, errorMessage)
	pipe.HDel(ctx, "ota", progressKey)
	pipe.HDel(ctx, "ota", downloadedKey)
	pipe.HDel(ctx, "ota", totalKey)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to set error for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set error for %s: type=%s, message=%s", r.component, errorType, errorMessage)
	return nil
}

// ClearError removes error and error-message keys from Redis
func (r *Reporter) ClearError(ctx context.Context) error {
	pipe := r.client.Pipeline()

	errorKey := fmt.Sprintf("error:%s", r.component)
	errorMessageKey := fmt.Sprintf("error-message:%s", r.component)

	pipe.HDel(ctx, "ota", errorKey)
	pipe.HDel(ctx, "ota", errorMessageKey)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to clear error for component %s: %w", r.component, err)
	}

	r.logger.Printf("Cleared error keys for %s", r.component)
	return nil
}

// SetDownloadProgress sets the download progress with both percentage and byte counts
func (r *Reporter) SetDownloadProgress(ctx context.Context, downloaded, total int64) error {
	pipe := r.client.Pipeline()

	progressKey := fmt.Sprintf("download-progress:%s", r.component)
	downloadedKey := fmt.Sprintf("download-bytes:%s", r.component)
	totalKey := fmt.Sprintf("download-total:%s", r.component)

	// Calculate percentage (0-100)
	var percentage int
	if total > 0 {
		percentage = int((downloaded * 100) / total)
	}

	pipe.HSet(ctx, "ota", progressKey, percentage)
	pipe.HSet(ctx, "ota", downloadedKey, downloaded)
	pipe.HSet(ctx, "ota", totalKey, total)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to set download progress for component %s: %w", r.component, err)
	}

	return nil
}

// ClearDownloadProgress removes the download progress keys from Redis
func (r *Reporter) ClearDownloadProgress(ctx context.Context) error {
	pipe := r.client.Pipeline()

	progressKey := fmt.Sprintf("download-progress:%s", r.component)
	downloadedKey := fmt.Sprintf("download-bytes:%s", r.component)
	totalKey := fmt.Sprintf("download-total:%s", r.component)

	pipe.HDel(ctx, "ota", progressKey)
	pipe.HDel(ctx, "ota", downloadedKey)
	pipe.HDel(ctx, "ota", totalKey)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to clear download progress for component %s: %w", r.component, err)
	}

	return nil
}

// SetUpdateMethod sets the update method being used (delta or full) in Redis
func (r *Reporter) SetUpdateMethod(ctx context.Context, method string) error {
	key := fmt.Sprintf("update-method:%s", r.component)

	err := r.client.HSet(ctx, "ota", key, method).Err()
	if err != nil {
		return fmt.Errorf("failed to set update method for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set update method for %s: %s", r.component, method)
	return nil
}

// ClearUpdateMethod removes the update method key from Redis
func (r *Reporter) ClearUpdateMethod(ctx context.Context) error {
	key := fmt.Sprintf("update-method:%s", r.component)

	err := r.client.HDel(ctx, "ota", key).Err()
	if err != nil {
		return fmt.Errorf("failed to clear update method for component %s: %w", r.component, err)
	}

	r.logger.Printf("Cleared update method for %s", r.component)
	return nil
}
