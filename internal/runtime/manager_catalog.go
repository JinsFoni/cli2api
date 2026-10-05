package runtime

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers/qoder"
)

// Per-account catalog snapshots on Pool items. Qoder uses worker /admin/models;
// in-process providers use adapter.Models(). Does not merge with API display cache.

const modelCatalogTTL = 5 * time.Minute

// EnsureModelCatalogs refreshes per-account catalogs that are missing or
// older than the worker TTL. Failures leave the previous snapshot in place
// and never flip readiness or cooldown.
//
// Refreshes run concurrently in the background with a bounded semaphore so
// the first chat request is never blocked by N serial 15-second timeouts
// when multiple accounts are offline. The caller returns immediately; a
// nil Models slice means unknown (fail open) until the background refresh
// completes and subsequent requests use the fresh catalog.
func (m *Manager) EnsureModelCatalogs(ctx context.Context, force bool) {
	if m == nil || m.pool == nil {
		return
	}
	now := time.Now()
	var stale []Item
	for _, item := range m.pool.Items() {
		if !force && item.Models != nil && !item.ModelsAt.IsZero() && now.Sub(item.ModelsAt) < modelCatalogTTL {
			continue
		}
		stale = append(stale, item)
	}
	if len(stale) == 0 {
		return
	}
	// Fire background refreshes concurrently with a bounded semaphore.
	// Use m.runCtx so refreshes survive the caller's request context and
	// are canceled only on shutdown.
	if ctx.Err() != nil {
		return // caller context already cancelled — no point firing goroutines
	}
	const maxConcurrent = 4
	sem := make(chan struct{}, maxConcurrent)
	for _, item := range stale {
		go func(it Item) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				m.fetchAccountModels(m.runCtx, it)
			case <-m.runCtx.Done():
			}
		}(item)
	}
}

func (m *Manager) fetchAccountModels(ctx context.Context, item Item) {
	if m == nil || m.pool == nil || item.ID == "" {
		return
	}
	if adapter, ok := m.providers.Get(item.Provider); ok && adapter.Models != nil {
		models, err := adapter.Models.Models(ctx, item.ID)
		if err != nil {
			var transport qoder.TransportError
			var statusErr qoder.HTTPStatusError
			switch {
			case errors.As(err, &statusErr):
				log.Printf("catalog refresh failed account=%s provider=%s stage=status status=%d body=%q", item.ID, item.Provider, statusErr.Status, statusErr.Body)
			case errors.As(err, &transport):
				log.Printf("catalog refresh failed account=%s provider=%s stage=http: %v", item.ID, item.Provider, err)
			default:
				log.Printf("catalog refresh failed account=%s provider=%s stage=decode: %v", item.ID, item.Provider, err)
			}
			return
		}
		m.pool.MergeModels(item.ID, qoder.CatalogIDsFromInfos(models))
	}
}
