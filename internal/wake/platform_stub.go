//go:build !windows

package wake

import (
	"fmt"
	"time"
)

// stubPlatform stands in on systems that are not Windows.
//
// It reports the truth rather than pretending: scheduled wake needs a Windows
// wake timer, and a non-Windows host running the agent for development or
// testing cannot offer one.
type stubPlatform struct{}

// NewPlatform returns the wake platform for this operating system.
func NewPlatform() Platform { return stubPlatform{} }

func (stubPlatform) SupportsModernStandby() bool { return false }

func (stubPlatform) WakeArmed() (bool, string) {
	return false, "Waking is only supported when ALL SHARE runs on Windows."
}

func (stubPlatform) ScheduleCheckIn(time.Duration) error {
	return fmt.Errorf("allshare/wake: scheduled wake is only available on Windows")
}

func (stubPlatform) PreventSleep(string) (func(), error) {
	return func() {}, nil
}
