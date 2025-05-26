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

// SetIdleAndClearVersion atomically sets status to idle and clears update version
func (r *Reporter) SetIdleAndClearVersion(ctx context.Context) error {
	pipe := r.client.Pipeline()
	
	statusKey := fmt.Sprintf("status:%s", r.component)
	versionKey := fmt.Sprintf("update-version:%s", r.component)
	
	pipe.HSet(ctx, "ota", statusKey, string(StatusIdle))
	pipe.HDel(ctx, "ota", versionKey)
	
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to set idle and clear version for component %s: %w", r.component, err)
	}
	
	r.logger.Printf("Set status to idle and cleared version for %s", r.component)
	return nil
}