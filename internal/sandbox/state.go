package sandbox

import "slices"

// states are the sandbox states in their order. A sandbox moves only to a
// later state, and gone and failed share the last place.
var states = []State{StateStarting, StateIdle, StateBusy, StateStopping, StateGone, StateFailed}

// LiveStates returns the states of a sandbox that still operates, in their
// order.
func LiveStates() []State {
	return slices.DeleteFunc(slices.Clone(states), State.Done)
}

// DoneStates returns the states of a sandbox that no longer operates.
func DoneStates() []State {
	return slices.DeleteFunc(slices.Clone(states), func(s State) bool { return !s.Done() })
}

// Rank orders the states: a sandbox moves only to a state of a higher rank.
// The done states share the last rank. An unknown state has the rank -1.
func (s State) Rank() int {
	live := LiveStates()
	if s.Done() {
		return len(live)
	}
	return slices.Index(live, s)
}
