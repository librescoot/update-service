package vehicle

import (
	"fmt"
	"time"

	"github.com/librescoot/update-service/internal/redis"
)

// Service represents the vehicle service client
type Service struct {
	redis           *redis.Client
	vehicleHashKey  string
	previousState   string
	stateRestored   bool
	rebootCheckChan chan struct{}
	dryRun          bool
}

// New creates a new vehicle service client
func New(redis *redis.Client, vehicleHashKey string, dryRun bool) *Service {
	return &Service{
		redis:           redis,
		vehicleHashKey:  vehicleHashKey,
		stateRestored:   true,
		rebootCheckChan: make(chan struct{}),
		dryRun:          dryRun,
	}
}

// SetUpdatingState sets the vehicle state to "updating" and saves the previous state
func (s *Service) SetUpdatingState() error {
	// Get current state
	currentState, err := s.redis.GetVehicleState(s.vehicleHashKey)
	if err != nil {
		return fmt.Errorf("failed to get current vehicle state: %w", err)
	}

	// Save current state
	s.previousState = currentState
	s.stateRestored = false

	// Set state to "updating"
	if err := s.redis.SetVehicleState(s.vehicleHashKey, "updating"); err != nil {
		return fmt.Errorf("failed to set vehicle state to updating: %w", err)
	}

	return nil
}

// RestorePreviousState restores the vehicle state to the previous state
func (s *Service) RestorePreviousState() error {
	if s.stateRestored {
		return nil
	}

	if err := s.redis.SetVehicleState(s.vehicleHashKey, s.previousState); err != nil {
		return fmt.Errorf("failed to restore previous vehicle state: %w", err)
	}

	s.stateRestored = true
	return nil
}

// GetCurrentState gets the current vehicle state
func (s *Service) GetCurrentState() (string, error) {
	return s.redis.GetVehicleState(s.vehicleHashKey)
}

// IsSafeForDbcUpdate checks if it's safe to update the DBC
// DBC updates should not turn off the DBC, but should allow locking
func (s *Service) IsSafeForDbcUpdate() (bool, error) {
	// Get current state
	_, err := s.redis.GetVehicleState(s.vehicleHashKey)
	if err != nil {
		return false, fmt.Errorf("failed to get current vehicle state: %w", err)
	}

	// DBC updates can be applied in any state
	// The vehicle service will handle preventing DBC power-off during updates
	return true, nil
}

// IsSafeForMdbReboot checks if it's safe to reboot the MDB
// MDB should only be rebooted when the scooter is in stand-by mode or shutting down
func (s *Service) IsSafeForMdbReboot() (bool, error) {
	// Get current state
	currentState, err := s.redis.GetVehicleState(s.vehicleHashKey)
	if err != nil {
		return false, fmt.Errorf("failed to get current vehicle state: %w", err)
	}

	// MDB can be rebooted in stand-by mode or when shutting down
	return currentState == "stand-by" || currentState == "shutting-down", nil
}

// IsSafeForDbcReboot checks if it's safe to reboot the DBC
// DBC should not be rebooted when the scooter is being actively used
func (s *Service) IsSafeForDbcReboot() (bool, error) {
	// Get current state
	currentState, err := s.redis.GetVehicleState(s.vehicleHashKey)
	if err != nil {
		return false, fmt.Errorf("failed to get current vehicle state: %w", err)
	}

	// DBC should not be rebooted in ready-to-drive or parked modes
	return currentState != "ready-to-drive" && currentState != "parked", nil
}

// IsSafeForDbcPowerDown checks if it's safe to power down the DBC
// Same logic as reboot - DBC should not be powered down when scooter is actively used
func (s *Service) IsSafeForDbcPowerDown() (bool, error) {
	// Get current state
	currentState, err := s.redis.GetVehicleState(s.vehicleHashKey)
	if err != nil {
		return false, fmt.Errorf("failed to get current vehicle state: %w", err)
	}

	// DBC should not be powered down in ready-to-drive or parked modes
	return currentState != "ready-to-drive" && currentState != "parked", nil
}

// ScheduleMdbRebootCheck schedules a check for MDB reboot safety
// It will periodically check if it's safe to reboot the MDB
// and signal on the rebootCheckChan when it's safe
func (s *Service) ScheduleMdbRebootCheck(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				safe, err := s.IsSafeForMdbReboot()
				if err != nil {
					// Log error and continue
					continue
				}

				if safe {
					// Signal that it's safe to reboot
					select {
					case s.rebootCheckChan <- struct{}{}:
						// Signal sent
						return
					default:
						// Channel not ready, continue
					}
				}
			}
		}
	}()
}

// WaitForSafeReboot waits for the MDB reboot check to signal that it's safe to reboot
// It returns true if it's safe to reboot, false if the context is cancelled
func (s *Service) WaitForSafeReboot() bool {
	select {
	case <-s.rebootCheckChan:
		return true
	}
}

// TriggerReboot triggers a reboot of the specified component
func (s *Service) TriggerReboot(component string) error {
	// Check if we're in dry-run mode
	if s.dryRun {
		// In dry-run mode, just log that we would reboot
		return fmt.Errorf("DRY-RUN: Would reboot %s, but dry-run mode is enabled", component)
	}

	// Handle component-specific reboot logic
	switch component {
	case "mdb":
		// For MDB, check if it's safe to reboot
		safe, err := s.IsSafeForMdbReboot()
		if err != nil {
			return fmt.Errorf("failed to check if safe for MDB reboot: %w", err)
		}

		if !safe {
			return fmt.Errorf("not safe to reboot MDB")
		}
		
		// Trigger MDB reboot via pm-service
		return s.redis.TriggerReboot()
		
	case "dbc":
		// Check if it's safe to reboot DBC
		safe, err := s.IsSafeForDbcReboot()
		if err != nil {
			return fmt.Errorf("failed to check if safe for DBC reboot: %w", err)
		}

		if !safe {
			currentState, _ := s.GetCurrentState() // Best effort to get state for the error message
			return fmt.Errorf("not safe to reboot DBC while in %s state", currentState)
		}
		
		// For DBC, we need to send complete-dbc followed by start-dbc
		// This properly restarts the DBC via vehicle-service
		if err := s.redis.PushUpdateCommand("complete-dbc"); err != nil {
			return fmt.Errorf("failed to send complete-dbc command: %w", err)
		}
		
		// Wait a short time for the complete-dbc command to be processed
		time.Sleep(500 * time.Millisecond)
		
		// Send start-dbc to initiate the reboot
		if err := s.redis.PushUpdateCommand("start-dbc"); err != nil {
			return fmt.Errorf("failed to send start-dbc command: %w", err)
		}
		
		return nil
		
	default:
		return fmt.Errorf("unknown component: %s", component)
	}
}
