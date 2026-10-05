package accounts

import (
	"fmt"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/proxy"
)

const (
	DefaultMaxInFlight = 4
	DefaultPriority    = 50
)

func DefaultMaxInFlightValue(value int) int {
	if value <= 0 {
		return DefaultMaxInFlight
	}
	return value
}

func DefaultPriorityValue(value int) int {
	if value <= 0 {
		return DefaultPriority
	}
	return value
}

func DefaultDropSystemPrompt(value *bool) bool {
	if value == nil {
		return true
	}
	return *value
}

func DefaultWorkBuddyAutoCheckin(value *bool) bool {
	if value == nil {
		return false
	}
	return *value
}

func ValidateModelContextLength(contextLength int) error {
	if contextLength < 0 || contextLength > 4_000_000 || (contextLength > 0 && contextLength < 1024) {
		return fmt.Errorf("context_length must be 0 or between 1024 and 4000000")
	}
	return nil
}

// ResolveWorkBuddyCheckinTime inherits defaultTime when value is empty.
func ResolveWorkBuddyCheckinTime(value, defaultTime string) (string, error) {
	if strings.TrimSpace(value) == "" {
		if strings.TrimSpace(defaultTime) == "" {
			return DefaultWorkBuddyCheckinTime, nil
		}
		return defaultTime, nil
	}
	return NormalizeWorkBuddyCheckinTime(value)
}

// ValidateAccountProxy enforces the per-provider proxy boundary. All
// providers are in-process now and the shared Go transport handles
// http(s)/SOCKS alike, so any scheme Parse accepts is valid. The former
// Qoder http-only rule existed because the Node worker's fetch could not
// use SOCKS; that constraint died with the worker.
func ValidateAccountProxy(providerID, region, raw string) error {
	if _, _, err := providers.Resolve(providerID, region); err != nil {
		return err
	}
	_, err := proxy.Parse(raw)
	return err
}
