package runtime

import (
	"context"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// In-process account lifecycle. All providers register RuntimeInProcess, so
// start/stop reduce to pool upsert/remove; no child processes are spawned.

func (m *Manager) ReplaceProxyAPIKey(ctx context.Context, key string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.config.ProxyAPIKey = key
	m.mu.Unlock()
	return nil
}

func (m *Manager) startAccount(ctx context.Context, account Account) error {
	descriptor, _, err := providers.Resolve(account.Provider, account.ProviderRegion)
	if err != nil {
		return err
	}
	if m.runCtx.Err() != nil {
		return errManagerClosed
	}
	m.pool.Upsert(Item{
		ID: account.ID, Provider: descriptor.ID, Region: account.ProviderRegion,
		Runtime: string(descriptor.Runtime), DropSystemPrompt: account.DropSystemPrompt,
		Weight: NormalizeWeight(account.Priority), MaxInFlight: account.MaxInFlight, Quota: account.Quota,
		RuntimeState: "starting",
	})
	return nil
}

func (m *Manager) ReloadProxyURL(ctx context.Context, value string) error {
	// Serialize reloads so concurrent PATCHes cannot interleave.
	m.proxyReloadMu.Lock()
	defer m.proxyReloadMu.Unlock()

	value = strings.TrimSpace(value)

	m.mu.Lock()
	unchanged := strings.TrimSpace(m.config.ProxyURL) == value
	if unchanged && !m.proxyReloadPending {
		m.mu.Unlock()
		return nil
	}
	m.config.ProxyURL = value
	m.proxyReloadPending = false
	m.mu.Unlock()
	return nil
}

func (m *Manager) stopAccount(id string) error {
	m.pool.Remove(id)
	return nil
}

func (m *Manager) AccountURL(id string) (string, bool) {
	item, ok := m.pool.ByID(id)
	return item.URL, ok
}
