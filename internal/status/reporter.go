package status

import (
	"context"
	"fmt"
	"log"

	ipc "github.com/librescoot/redis-ipc"
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

// Reporter handles Redis status reporting for OTA updates using HashPublisher
type Reporter struct {
	pub       *ipc.HashPublisher
	component string
	logger    *log.Logger
}

// NewReporter creates a new status reporter for the given component
func NewReporter(client *ipc.Client, component string, logger *log.Logger) *Reporter {
	return &Reporter{
		pub:       client.NewHashPublisher("ota"),
		component: component,
		logger:    logger,
	}
}

// SetStatus updates the status for this component in Redis
func (r *Reporter) SetStatus(ctx context.Context, status Status) error {
	key := fmt.Sprintf("status:%s", r.component)

	err := r.pub.Set(key, string(status))
	if err != nil {
		return fmt.Errorf("failed to set status for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set status for %s: %s", r.component, status)
	return nil
}

// SetUpdateVersion updates the target update version for this component in Redis
func (r *Reporter) SetUpdateVersion(ctx context.Context, version string) error {
	key := fmt.Sprintf("update-version:%s", r.component)

	err := r.pub.Set(key, version)
	if err != nil {
		return fmt.Errorf("failed to set update version for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set update version for %s: %s", r.component, version)
	return nil
}

// ClearUpdateVersion removes the update version for this component from Redis
func (r *Reporter) ClearUpdateVersion(ctx context.Context) error {
	key := fmt.Sprintf("update-version:%s", r.component)

	err := r.pub.Delete(key)
	if err != nil {
		return fmt.Errorf("failed to clear update version for component %s: %w", r.component, err)
	}

	r.logger.Printf("Cleared update version for %s", r.component)
	return nil
}

// GetStatus retrieves the current status for this component from Redis
func (r *Reporter) GetStatus(ctx context.Context) (Status, error) {
	key := fmt.Sprintf("status:%s", r.component)

	result, err := r.pub.Get(key)
	if err != nil {
		return StatusIdle, nil // Default to idle if not set
	}

	return Status(result), nil
}

// SetStatusAndVersion atomically sets both status and update version
func (r *Reporter) SetStatusAndVersion(ctx context.Context, status Status, version string) error {
	statusKey := fmt.Sprintf("status:%s", r.component)
	versionKey := fmt.Sprintf("update-version:%s", r.component)

	err := r.pub.SetMany(map[string]any{
		statusKey:  string(status),
		versionKey: version,
	})
	if err != nil {
		return fmt.Errorf("failed to set status and version for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set status and version for %s: %s, %s", r.component, status, version)
	return nil
}

// SetIdleAndClearVersion atomically sets status to idle and clears update version, error, progress, and method keys
func (r *Reporter) SetIdleAndClearVersion(ctx context.Context) error {
	statusKey := fmt.Sprintf("status:%s", r.component)
	versionKey := fmt.Sprintf("update-version:%s", r.component)
	errorKey := fmt.Sprintf("error:%s", r.component)
	errorMessageKey := fmt.Sprintf("error-message:%s", r.component)
	progressKey := fmt.Sprintf("download-progress:%s", r.component)
	downloadedKey := fmt.Sprintf("download-bytes:%s", r.component)
	totalKey := fmt.Sprintf("download-total:%s", r.component)
	methodKey := fmt.Sprintf("update-method:%s", r.component)

	// Set status to idle
	if err := r.pub.Set(statusKey, string(StatusIdle)); err != nil {
		return fmt.Errorf("failed to set idle status for component %s: %w", r.component, err)
	}

	// Delete the other keys
	for _, key := range []string{versionKey, errorKey, errorMessageKey, progressKey, downloadedKey, totalKey, methodKey} {
		_ = r.pub.Delete(key) // Ignore errors on delete
	}

	r.logger.Printf("Set status to idle and cleared version for %s", r.component)
	return nil
}

// SetError atomically sets status to error, stores error details, and clears download progress
func (r *Reporter) SetError(ctx context.Context, errorType, errorMessage string) error {
	statusKey := fmt.Sprintf("status:%s", r.component)
	errorKey := fmt.Sprintf("error:%s", r.component)
	errorMessageKey := fmt.Sprintf("error-message:%s", r.component)
	progressKey := fmt.Sprintf("download-progress:%s", r.component)
	downloadedKey := fmt.Sprintf("download-bytes:%s", r.component)
	totalKey := fmt.Sprintf("download-total:%s", r.component)

	// Set error status and details
	err := r.pub.SetMany(map[string]any{
		statusKey:       string(StatusError),
		errorKey:        errorType,
		errorMessageKey: errorMessage,
	})
	if err != nil {
		return fmt.Errorf("failed to set error for component %s: %w", r.component, err)
	}

	// Clear download progress keys
	for _, key := range []string{progressKey, downloadedKey, totalKey} {
		_ = r.pub.Delete(key) // Ignore errors on delete
	}

	r.logger.Printf("Set error for %s: type=%s, message=%s", r.component, errorType, errorMessage)
	return nil
}

// ClearError removes error and error-message keys from Redis
func (r *Reporter) ClearError(ctx context.Context) error {
	errorKey := fmt.Sprintf("error:%s", r.component)
	errorMessageKey := fmt.Sprintf("error-message:%s", r.component)

	_ = r.pub.Delete(errorKey)
	_ = r.pub.Delete(errorMessageKey)

	r.logger.Printf("Cleared error keys for %s", r.component)
	return nil
}

// SetDownloadProgress sets the download progress with both percentage and byte counts
func (r *Reporter) SetDownloadProgress(ctx context.Context, downloaded, total int64) error {
	progressKey := fmt.Sprintf("download-progress:%s", r.component)
	downloadedKey := fmt.Sprintf("download-bytes:%s", r.component)
	totalKey := fmt.Sprintf("download-total:%s", r.component)

	// Calculate percentage (0-100)
	var percentage int
	if total > 0 {
		percentage = int((downloaded * 100) / total)
	}

	err := r.pub.SetMany(map[string]any{
		progressKey:   percentage,
		downloadedKey: downloaded,
		totalKey:      total,
	})
	if err != nil {
		return fmt.Errorf("failed to set download progress for component %s: %w", r.component, err)
	}

	return nil
}

// SetInstallProgress sets the install/delta application progress (0-100)
func (r *Reporter) SetInstallProgress(ctx context.Context, percent int) error {
	progressKey := fmt.Sprintf("install-progress:%s", r.component)
	err := r.pub.Set(progressKey, percent)
	if err != nil {
		return fmt.Errorf("failed to set install progress for component %s: %w", r.component, err)
	}
	return nil
}

// ClearInstallProgress removes the install progress key from Redis
func (r *Reporter) ClearInstallProgress(ctx context.Context) error {
	progressKey := fmt.Sprintf("install-progress:%s", r.component)
	return r.pub.Delete(progressKey)
}

// ClearDownloadProgress removes the download progress keys from Redis
func (r *Reporter) ClearDownloadProgress(ctx context.Context) error {
	progressKey := fmt.Sprintf("download-progress:%s", r.component)
	downloadedKey := fmt.Sprintf("download-bytes:%s", r.component)
	totalKey := fmt.Sprintf("download-total:%s", r.component)

	_ = r.pub.Delete(progressKey)
	_ = r.pub.Delete(downloadedKey)
	_ = r.pub.Delete(totalKey)

	return nil
}

// SetUpdateMethod sets the update method being used (delta or full) in Redis
func (r *Reporter) SetUpdateMethod(ctx context.Context, method string) error {
	key := fmt.Sprintf("update-method:%s", r.component)

	err := r.pub.Set(key, method)
	if err != nil {
		return fmt.Errorf("failed to set update method for component %s: %w", r.component, err)
	}

	r.logger.Printf("Set update method for %s: %s", r.component, method)
	return nil
}

// ClearUpdateMethod removes the update method key from Redis
func (r *Reporter) ClearUpdateMethod(ctx context.Context) error {
	key := fmt.Sprintf("update-method:%s", r.component)

	err := r.pub.Delete(key)
	if err != nil {
		return fmt.Errorf("failed to clear update method for component %s: %w", r.component, err)
	}

	r.logger.Printf("Cleared update method for %s", r.component)
	return nil
}

// Initialize sets initial values for OTA keys on service startup
func (r *Reporter) Initialize(ctx context.Context, updateMethod string) error {
	statusKey := fmt.Sprintf("status:%s", r.component)
	progressKey := fmt.Sprintf("download-progress:%s", r.component)
	methodKey := fmt.Sprintf("update-method:%s", r.component)

	err := r.pub.SetMany(map[string]any{
		statusKey:   string(StatusIdle),
		progressKey: 0,
		methodKey:   updateMethod,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize OTA keys for component %s: %w", r.component, err)
	}

	r.logger.Printf("Initialized OTA keys for %s (status: idle, progress: 0, method: %s)", r.component, updateMethod)
	return nil
}
