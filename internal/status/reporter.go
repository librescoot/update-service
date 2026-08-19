package status

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

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
//
// The flat `status` and `update-type` fields on the same hash, for consumers
// that follow the stock convention, are not written here: see FlatFor and
// FlatMirror for how the two components' statuses combine into that pair.
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
	m := map[string]any{
		r.key("status"):            string(StatusIdle),
		r.key("update-version"):    "",
		r.key("update-method"):     "",
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  "",
		r.key("error"):             "",
		r.key("error-message"):     "",
	}
	err := r.pub.SetMany(m, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set idle for %s: %w", r.component, err)
	}
	r.logger.Printf("Set idle for %s", r.component)
	return nil
}

// SetAborted returns the component to idle after a download was abandoned for
// being too slow, recording why and how many subsequent checks to skip.
//
// This is deliberately not SetIdle plus two writes: SetIdle clears
// download-bytes and download-total, which are exactly the fields an abort
// wants to preserve so the partial's progress stays visible. It is also a
// single SetMany so no consumer sees idle-without-a-reason.
//
// A zero skipChecks clears the field, meaning no backoff applies.
func (r *Reporter) SetAborted(ctx context.Context, reason string, skipChecks int) error {
	skip := ""
	if skipChecks > 0 {
		skip = strconv.Itoa(skipChecks)
	}
	m := map[string]any{
		r.key("status"):                string(StatusIdle),
		r.key("update-version"):        "",
		r.key("update-method"):         "",
		r.key("install-progress"):      "",
		r.key("error"):                 "",
		r.key("error-message"):         "",
		r.key("download-abort-reason"): reason,
		r.key("download-skip-checks"):  skip,
	}
	if err := r.pub.SetMany(m, ipc.Sync()); err != nil {
		return fmt.Errorf("set aborted for %s: %w", r.component, err)
	}
	r.logger.Printf("Download abandoned for %s (%s), skip_checks=%q", r.component, reason, skip)
	return nil
}

// SetHeartbeat records that a long-running operation is still alive. Written
// periodically for the whole duration of downloading, retry waits, preparing
// and installing, so vehicle-service can tell a wedged DBC update from one
// that is merely between retries. Async on purpose: it is frequent and the
// next tick corrects any staleness.
func (r *Reporter) SetHeartbeat(ctx context.Context, t time.Time) error {
	return r.pub.Set(r.key("heartbeat"), strconv.FormatInt(t.Unix(), 10))
}

// ClearHeartbeat clears the liveness marker when a long-running operation
// ends. Without this a stale heartbeat from a completed operation looks
// alive to a consumer (vehicle-service seeds a watchdog flag from this field
// at startup) until the field happens to be overwritten by some later
// operation, which may be a long time coming or may never happen if the
// component is downgraded to an image that no longer writes heartbeats.
func (r *Reporter) ClearHeartbeat(ctx context.Context) error {
	return r.pub.Set(r.key("heartbeat"), "")
}

// SetDownloading atomically sets downloading status with version, method,
// and resets all progress to 0.
func (r *Reporter) SetDownloading(ctx context.Context, version, method string) error {
	m := map[string]any{
		r.key("status"):                string(StatusDownloading),
		r.key("update-version"):        version,
		r.key("update-method"):         method,
		r.key("download-progress"):     0,
		r.key("download-bytes"):        0,
		r.key("download-total"):        0,
		r.key("install-progress"):      0,
		r.key("error"):                 "",
		r.key("error-message"):         "",
		r.key("download-abort-reason"): "",
		r.key("download-skip-checks"):  "",
	}
	err := r.pub.SetMany(m, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set downloading for %s: %w", r.component, err)
	}
	r.logger.Printf("Set downloading for %s: version=%s method=%s", r.component, version, method)
	return nil
}

// SetPreparing atomically transitions to preparing status, clears download
// progress fields, and resets install progress to 0.
func (r *Reporter) SetPreparing(ctx context.Context) error {
	m := map[string]any{
		r.key("status"):            string(StatusPreparing),
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  0,
	}
	err := r.pub.SetMany(m, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set preparing for %s: %w", r.component, err)
	}
	r.logger.Printf("Set preparing for %s", r.component)
	return nil
}

// SetInstalling atomically transitions to installing status, clears download
// progress fields, and resets install progress to 0.
func (r *Reporter) SetInstalling(ctx context.Context) error {
	m := map[string]any{
		r.key("status"):            string(StatusInstalling),
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  0,
	}
	err := r.pub.SetMany(m, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set installing for %s: %w", r.component, err)
	}
	r.logger.Printf("Set installing for %s", r.component)
	return nil
}

// SetPendingReboot atomically transitions to pending-reboot status and
// clears progress fields.
func (r *Reporter) SetPendingReboot(ctx context.Context) error {
	m := map[string]any{
		r.key("status"):            string(StatusPendingReboot),
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  "",
	}
	err := r.pub.SetMany(m, ipc.Sync())
	if err != nil {
		return fmt.Errorf("set pending-reboot for %s: %w", r.component, err)
	}
	r.logger.Printf("Set pending-reboot for %s", r.component)
	return nil
}

// SetError atomically transitions to error status with error details and
// clears progress fields.
func (r *Reporter) SetError(ctx context.Context, errorType, errorMessage string) error {
	m := map[string]any{
		r.key("status"):            string(StatusError),
		r.key("error"):             errorType,
		r.key("error-message"):     errorMessage,
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  "",
	}
	err := r.pub.SetMany(m, ipc.Sync())
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

// SetSkipChecksRemaining updates just the download-skip-checks field to the
// count actually remaining after a skip was just served. SetAborted writes
// the initial count when the backoff is first recorded, but nothing updated
// it afterwards: it froze at that value until the next attempt cleared it,
// which made the published field describe a backoff that was not actually
// being drawn down. This is what makes it track reality: called from every
// ShouldSkip call site, every time a skip is served, with the exact remaining
// value ShouldSkip returned. A remaining of 0 clears the field.
func (r *Reporter) SetSkipChecksRemaining(ctx context.Context, remaining int) error {
	skip := ""
	if remaining > 0 {
		skip = strconv.Itoa(remaining)
	}
	return r.pub.Set(r.key("download-skip-checks"), skip)
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
	// download-abort-reason and download-skip-checks are deliberately absent:
	// they mirror on-disk backoff state that outlives the process. Initialize
	// runs on every service start, which for the DBC is every dashboard
	// power-on, so clearing them here would wipe the orchestrator's backoff
	// gate on every ride.
	//
	// The preview-* fields are cleared: a preview answers "what would a switch
	// to this channel fetch right now", so one left over from before a restart
	// is not an answer, it is a guess with a plausible shape.
	m := map[string]any{
		r.key("status"):            string(StatusIdle),
		r.key("update-method"):     updateMethod,
		r.key("download-progress"): "",
		r.key("download-bytes"):    "",
		r.key("download-total"):    "",
		r.key("install-progress"):  "",
		r.key("error"):             "",
		r.key("error-message"):     "",
		r.key("preview-channel"):   "",
		r.key("preview-status"):    "",
		r.key("preview-version"):   "",
		r.key("preview-size"):      "",
	}
	err := r.pub.SetMany(m, ipc.Sync())
	if err != nil {
		return fmt.Errorf("initialize OTA keys for %s: %w", r.component, err)
	}
	r.logger.Printf("Initialized OTA keys for %s (status: idle, method: %s)", r.component, updateMethod)
	return nil
}

// --- Channel preview ---
//
// A preview answers "what would switching to this channel cost" without
// touching the update state machine: it only ever writes preview-* fields, so
// a preview issued while an update is downloading cannot disturb it. The
// channel is echoed back on every write so a consumer can tell the answer it
// is reading apart from a stale one for a channel it no longer cares about.

// PreviewStatus values published in preview-status:{component}.
const (
	PreviewChecking    = "checking"
	PreviewReady       = "ready"
	PreviewUnavailable = "unavailable"
	PreviewError       = "error"
)

// SetPreviewChecking marks a preview of channel as in flight and clears the
// result fields of whatever the previous preview left behind.
func (r *Reporter) SetPreviewChecking(ctx context.Context, channel string) error {
	m := map[string]any{
		r.key("preview-channel"): channel,
		r.key("preview-status"):  PreviewChecking,
		r.key("preview-version"): "",
		r.key("preview-size"):    "",
	}
	if err := r.pub.SetMany(m, ipc.Sync()); err != nil {
		return fmt.Errorf("set preview checking for %s: %w", r.component, err)
	}
	r.logger.Printf("Preview for %s: checking %s", r.component, channel)
	return nil
}

// SetPreviewResult publishes a finished preview. version and size are only
// meaningful for PreviewReady; the other statuses pass them empty and zero.
func (r *Reporter) SetPreviewResult(ctx context.Context, channel, previewStatus, version string, size int64) error {
	sizeStr := ""
	if size > 0 {
		sizeStr = strconv.FormatInt(size, 10)
	}
	m := map[string]any{
		r.key("preview-channel"): channel,
		r.key("preview-status"):  previewStatus,
		r.key("preview-version"): version,
		r.key("preview-size"):    sizeStr,
	}
	if err := r.pub.SetMany(m, ipc.Sync()); err != nil {
		return fmt.Errorf("set preview result for %s: %w", r.component, err)
	}
	r.logger.Printf("Preview for %s: channel=%s status=%s version=%s size=%s",
		r.component, channel, previewStatus, version, sizeStr)
	return nil
}
