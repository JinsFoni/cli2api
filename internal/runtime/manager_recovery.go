package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Account lifecycle recovery. In-process accounts cannot "exit", but a start
// failure (unknown provider/region, closed manager) still records a dead
// state so the operator sees it and disable/delete clears it.

func (m *Manager) startAccountWithRecovery(ctx context.Context, account Account) error {
	err := m.startAccount(ctx, account)
	if err != nil && !errors.Is(err, errManagerClosed) && m.runCtx.Err() == nil {
		m.beginAccountRecovery(account.ID, err)
	}
	return err
}

func (m *Manager) beginAccountRecovery(id string, startErr error) {
	if m == nil || m.store == nil || strings.TrimSpace(id) == "" {
		return
	}
	message := "account failed to start"
	if startErr != nil {
		message = startErr.Error()
	}
	m.mu.Lock()
	if m.runCtx.Err() != nil {
		m.mu.Unlock()
		return
	}
	m.restarts[id]++
	m.mu.Unlock()
	_ = m.store.Observe(context.Background(), id, "", "dead", message, KindUnavailable)
	m.pool.SetRuntimeState(id, "dead", time.Time{}, 0, message)
}

func (m *Manager) resetRestartBackoff(id string) {
	if m == nil || id == "" {
		return
	}
	m.mu.Lock()
	delete(m.restartBackoff, id)
	m.mu.Unlock()
}
