// Package game defines durable game-level contracts that are independent of
// exchange mechanics and transport concerns.
package game

import "fmt"

type PlayMode string

const (
	PlayModeTurnBased PlayMode = "turn_based"
	PlayModeRealTime  PlayMode = "real_time"
)

func (m PlayMode) Validate() error {
	switch m {
	case PlayModeTurnBased, PlayModeRealTime:
		return nil
	default:
		return fmt.Errorf("unsupported play mode %q", m)
	}
}

type LifecycleState string

const (
	LifecyclePreparing LifecycleState = "preparing"
	LifecycleCountdown LifecycleState = "countdown"
	LifecycleRunning   LifecycleState = "running"
	LifecyclePaused    LifecycleState = "paused"
	LifecycleCompleted LifecycleState = "completed"
)

const (
	LifecycleVersion uint32 = 1
	GeneratorVersion uint32 = 1
)

func (s LifecycleState) Validate() error {
	switch s {
	case LifecyclePreparing, LifecycleCountdown, LifecycleRunning, LifecyclePaused, LifecycleCompleted:
		return nil
	default:
		return fmt.Errorf("unsupported lifecycle state %q", s)
	}
}
