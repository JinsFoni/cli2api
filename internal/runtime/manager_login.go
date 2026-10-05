package runtime

import (
	"context"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// WorkerAdmin is retired with the worker transport. Login flows are native
// (PAT) or in-process adapters; no admin HTTP proxying remains.
func (m *Manager) WorkerAdmin(ctx context.Context, input providers.AdminRequest) (providers.AdminResponse, error) {
	return providers.AdminResponse{}, &providers.ActionError{Code: "not_found", Err: providers.ErrUnsupported}
}
