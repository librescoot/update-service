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
	StatusIdle          Status = "idle"
	StatusDownloading   Status = "downloading"
	StatusPreparing     Status = "preparing"
	StatusInstalling    Status = "installing"
	StatusPendingReboot Status = "pending-reboot"
	StatusError         Status = "error"
)

// Reporter handles Redis status reporting for OTA updates using HashPublisher.
//
// Every state transition writes all affected fields in a single SetMany call,
// so consumers (e.g. "lsc ota watch") never see an inconsistent snapshot such
// as status=downloading without download-progress.
//
// State transitions use ipc.Sync() so they are ordered in Redis in the order
// the caller issued them. Without this, back-to-back transitions like
// SetDownloading followed by SetInstalling for a local-file install can land
// in reverse order (HashPublisher async fires one goroutine per call with no
// ordering guarantee), leaving Redis stuck on "downloading" while the service
// is actually installing. Partial progress updates stay async because they
// are frequent and eventually consistent: the next update corrects any
// staleness.
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

// key returns a namespaced Redis hash field for this component.
func (r *Reporter) key(field string) string {
	return fmt.Sprintf("%s:%s", field, r.component)
}

// --- Read ---

// GetStatus retrieves the current status for this component from Redis
func (r *Reporter) GetStatus(ctx context.Context) (Status, error) {
	result, err := r.pub.Get(r.key("status"))
	if err != nil {
		return StatusIdle, nil // Default to idle if not set
	}
	return Status(result), nil
}

// --- Atomic state transitions ---

// SetIdle atomically sets status to idle and clears all other fields.
func (r *Reporter) SetIdle(ctx context.Context) error {
	err := r.pub.SetMany(map[string]any{
		r.key("status"):            string(StatusIdle),
		r.key("update-version"):    "",
		r.key("update-method"):     "",
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  "",
		r.key("error"):             "",
		r.key("error-message"):     "",
	}, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set idle for %s: %w", r.component, err)
	}
	r.logger.Printf("Set idle for %s", r.component)
	return nil
}

// SetDownloading atomically sets downloading status with version, method,
// and resets all progress to 0.
func (r *Reporter) SetDownloading(ctx context.Context, version, method string) error {
	err := r.pub.SetMany(map[string]any{
		r.key("status"):            string(StatusDownloading),
		r.key("update-version"):    version,
		r.key("update-method"):     method,
		r.key("download-progress"): 0,
		r.key("download-bytes"):    0,
		r.key("download-total"):    0,
		r.key("install-progress"):  0,
		r.key("error"):             "",
		r.key("error-message"):     "",
	}, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set downloading for %s: %w", r.component, err)
	}
	r.logger.Printf("Set downloading for %s: version=%s method=%s", r.component, version, method)
	return nil
}

// SetPreparing atomically transitions to preparing status, clears download
// progress fields, and resets install progress to 0.
func (r *Reporter) SetPreparing(ctx context.Context) error {
	err := r.pub.SetMany(map[string]any{
		r.key("status"):            string(StatusPreparing),
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  0,
	}, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set preparing for %s: %w", r.component, err)
	}
	r.logger.Printf("Set preparing for %s", r.component)
	return nil
}

// SetInstalling atomically transitions to installing status, clears download
// progress fields, and resets install progress to 0.
func (r *Reporter) SetInstalling(ctx context.Context) error {
	err := r.pub.SetMany(map[string]any{
		r.key("status"):            string(StatusInstalling),
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  0,
	}, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set installing for %s: %w", r.component, err)
	}
	r.logger.Printf("Set installing for %s", r.component)
	return nil
}

// SetPendingReboot atomically transitions to pending-reboot status and
// clears progress fields.
func (r *Reporter) SetPendingReboot(ctx context.Context) error {
	err := r.pub.SetMany(map[string]any{
		r.key("status"):            string(StatusPendingReboot),
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  "",
	}, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set pending-reboot for %s: %w", r.component, err)
	}
	r.logger.Printf("Set pending-reboot for %s", r.component)
	return nil
}

// SetError atomically transitions to error status with error details and
// clears progress fields.
func (r *Reporter) SetError(ctx context.Context, errorType, errorMessage string) error {
	err := r.pub.SetMany(map[string]any{
		r.key("status"):            string(StatusError),
		r.key("error"):             errorType,
		r.key("error-message"):     errorMessage,
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  "",
	}, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set error for %s: %w", r.component, err)
	}
	r.logger.Printf("Set error for %s: type=%s message=%s", r.component, errorType, errorMessage)
	return nil
}

// --- Partial updates (don't change status) ---

// SetDownloadProgress updates the download progress fields.
// Status is not changed — this is a partial update called frequently during download.
func (r *Reporter) SetDownloadProgress(ctx context.Context, downloaded, total int64) error {
	var percentage int
	if total > 0 {
		percentage = int((downloaded * 100) / total)
	}
	return r.pub.SetMany(map[string]any{
		r.key("download-progress"): percentage,
		r.key("download-bytes"):    downloaded,
		r.key("download-total"):    total,
	})
}

// SetInstallProgress updates the install/delta application progress (0-100).
// Status is not changed — this is a partial update called frequently during install.
func (r *Reporter) SetInstallProgress(ctx context.Context, percent int) error {
	return r.pub.Set(r.key("install-progress"), percent)
}

// SetUpdateVersion updates the target version without changing other fields.
// Used when the target version changes mid-update (e.g., additional deltas found).
func (r *Reporter) SetUpdateVersion(ctx context.Context, version string) error {
	err := r.pub.Set(r.key("update-version"), version, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set update version for %s: %w", r.component, err)
	}
	r.logger.Printf("Set update version for %s: %s", r.component, version)
	return nil
}

// --- Startup ---

// Initialize sets initial values for OTA keys on service startup.
func (r *Reporter) Initialize(ctx context.Context, updateMethod string) error {
	err := r.pub.SetMany(map[string]any{
		r.key("status"):            string(StatusIdle),
		r.key("update-method"):     updateMethod,
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  "",
		r.key("error"):             "",
		r.key("error-message"):     "",
	}, ipc.Sync())
	if err != nil {
		return fmt.Errorf("initialize OTA keys for %s: %w", r.component, err)
	}
	r.logger.Printf("Initialized OTA keys for %s (status: idle, method: %s)", r.component, updateMethod)
	return nil
}
