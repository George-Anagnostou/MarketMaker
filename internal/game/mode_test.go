package game

import "testing"

func TestPlayModeValidation(t *testing.T) {
	for _, mode := range []PlayMode{PlayModeTurnBased, PlayModeRealTime} {
		if err := mode.Validate(); err != nil {
			t.Fatalf("valid mode %q rejected: %v", mode, err)
		}
	}
	for _, mode := range []PlayMode{"", "realtime", "other"} {
		if err := mode.Validate(); err == nil {
			t.Fatalf("invalid mode %q accepted", mode)
		}
	}
}

func TestLifecycleValidation(t *testing.T) {
	for _, state := range []LifecycleState{LifecyclePreparing, LifecycleCountdown, LifecycleRunning, LifecyclePaused, LifecycleCompleted} {
		if err := state.Validate(); err != nil {
			t.Fatalf("valid lifecycle %q rejected: %v", state, err)
		}
	}
	for _, state := range []LifecycleState{"", "other"} {
		if err := state.Validate(); err == nil {
			t.Fatalf("invalid lifecycle %q accepted", state)
		}
	}
}
