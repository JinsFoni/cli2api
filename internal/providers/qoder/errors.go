package qoder

import (
	"errors"
	"fmt"
)

// Shared transport error types. ErrAccountNotRunning reports a lookup miss for
// an account that is enabled in the store but absent from the runtime pool.
var ErrAccountNotRunning = errors.New("account is disabled or not running")

type TransportError struct {
	Err error
}

func (e TransportError) Error() string {
	if e.Err == nil {
		return "provider transport error"
	}
	return e.Err.Error()
}

func (e TransportError) Unwrap() error { return e.Err }

type HTTPStatusError struct {
	Op     string
	Status int
	Body   string
}

func (e HTTPStatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("provider %s status %d body=%q", e.Op, e.Status, e.Body)
	}
	return fmt.Sprintf("provider %s status %d", e.Op, e.Status)
}

// StatusErrorStatus extracts the HTTP status from an HTTPStatusError.
func StatusErrorStatus(err error) int {
	var statusErr HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status
	}
	return 0
}

// Health is the pool health projection shared by probe paths.
type Health struct {
	OK        bool   `json:"ok"`
	Ready     bool   `json:"ready"`
	Hot       bool   `json:"hot"`
	UID       string `json:"uid"`
	InFlight  int    `json:"inFlight"`
	LastError string `json:"lastError"`
}
