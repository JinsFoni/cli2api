package qoder

import (
	"context"
)

// DisplayCatalog is the console display-catalog source. Native fetches the
// model catalog in-process per account (native signed GET); there is no
// worker transport anymore.
type DisplayCatalog struct {
	Native func(ctx context.Context, accountID string, refresh bool) ([]map[string]any, error)
}

func (s DisplayCatalog) Models(ctx context.Context, id string, refresh bool) ([]map[string]any, error) {
	if s.Native == nil {
		return nil, nil
	}
	return s.Native(ctx, id, refresh)
}
