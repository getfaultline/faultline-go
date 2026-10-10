package faultline

import "time"

// ResetForTest closes the client and clears the global scope so tests do not leak state.
func ResetForTest() {
	hub.Close(time.Second)
	hub.RootScope().Clear()
}
