package agnes

import (
	"context"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Quota reports Agnes usage. The platform has no usage endpoint (billing lives
// in the dashboard only), so the snapshot is always nil — the console card
// shows "unknown" rather than a wrong number.
func (c *Client) Quota(ctx context.Context, accountID string) (*providers.QuotaInfo, error) {
	return nil, nil
}
