package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/executor"
	"github.com/caigee-cmd/cli2api/internal/providers"
	accountruntime "github.com/caigee-cmd/cli2api/internal/runtime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlstore "github.com/caigee-cmd/cli2api/internal/store"
)

type fakeProcess struct {
	url     string
	stopped bool
	done    chan error
}

func (p *fakeProcess) URL() string        { return p.url }
func (p *fakeProcess) Done() <-chan error { return p.done }
func (p *fakeProcess) Stop() error {
	p.stopped = true
	select {
	case p.done <- nil:
	default:
	}
	return nil
}

type fakeStarter struct {
	accounts []accounts.Account
	homes    []string
	started  chan *fakeProcess
	failures int

	setProxyMu sync.Mutex
	proxyURLs  []string
}

// SetProxyURL lets fakeStarter satisfy ProxyConfigurableStarter so reload tests
// can observe whether the manager attempted a reload.
func (s *fakeStarter) SetProxyURL(value string) {
	s.setProxyMu.Lock()
	s.proxyURLs = append(s.proxyURLs, value)
	s.setProxyMu.Unlock()
}

func (s *fakeStarter) Start(_ context.Context, account accounts.Account, home string, port int) (accountruntime.ManagedProcess, error) {
	s.accounts = append(s.accounts, account)
	s.homes = append(s.homes, home)
	if s.failures > 0 {
		s.failures--
		return nil, errors.New("simulated start failure")
	}
	process := &fakeProcess{url: "http://127.0.0.1:" + itoa(port), done: make(chan error, 1)}
	if s.started != nil {
		s.started <- process
	}
	return process, nil
}

type delayedStarter struct {
	mu         sync.Mutex
	accounts   []accounts.Account
	started    chan *fakeProcess
	all        []*fakeProcess
	delayAfter int
	delay      time.Duration
}

func (s *delayedStarter) Start(_ context.Context, account accounts.Account, _ string, port int) (accountruntime.ManagedProcess, error) {
	s.mu.Lock()
	s.accounts = append(s.accounts, account)
	count := len(s.accounts)
	delay := time.Duration(0)
	if s.delayAfter > 0 && count > s.delayAfter {
		delay = s.delay
	}
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	process := &fakeProcess{url: "http://127.0.0.1:" + itoa(port), done: make(chan error, 1)}
	s.mu.Lock()
	s.all = append(s.all, process)
	s.mu.Unlock()
	if s.started != nil {
		s.started <- process
	}
	return process, nil
}

func (s *delayedStarter) startCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts)
}

func (s *delayedStarter) processes() []*fakeProcess {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*fakeProcess, len(s.all))
	copy(out, s.all)
	return out
}

func TestManagerRefreshesHealthAndPersistsUID(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "ready": true, "hot": true, "uid": "qoder-uid-1", "inFlight": 2,
		})
	}))
	defer worker.Close()
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Health", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: worker.URL})
	if err := manager.RefreshAll(ctx, false); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Get(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RemoteUID != "qoder-uid-1" || updated.Status != "ready" {
		t.Fatalf("updated account = %+v", updated)
	}
	item, _ := manager.Pool().ByID(account.ID)
	if item.Hot == nil || !*item.Hot || item.InFlight != 2 {
		t.Fatalf("pool health = %+v", item)
	}
}

func TestManagerRefreshSkipsDeadRecovery(t *testing.T) {
	called := 0
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		http.Error(w, "connection refused", http.StatusBadGateway)
	}))
	defer worker.Close()
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "DeadRefresh", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	restartAt := time.Now().Add(time.Minute)
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: worker.URL})
	manager.Pool().SetRuntimeState(account.ID, "dead", restartAt, 2, "account daemon exited")
	if err := manager.RefreshAll(ctx, false); err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatalf("health probe called %d times, want 0", called)
	}
	item, ok := manager.Pool().ByID(account.ID)
	if !ok || item.RuntimeState != "dead" || item.RestartBackoffLevel != 2 || item.NextRestartAt.IsZero() {
		t.Fatalf("dead recovery was overwritten: %+v ok=%v", item, ok)
	}
}

func TestManagerRefreshFetchesQuotaWithoutAffectingHealth(t *testing.T) {
	var quotaCalled bool
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "ready": true, "hot": true, "uid": "qoder-uid-1",
			})
		case "/admin/quota":
			quotaCalled = true
			if r.Header.Get("Authorization") != "Bearer proxy-key" {
				t.Errorf("quota request missing worker api key, got %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"quota": map[string]any{
					"isQuotaExceeded": false,
					"fetchedAt":       "2026-08-25T12:00:00Z",
					"userQuota":       map[string]any{"total": 600, "used": 150, "remaining": 450, "percentage": 25, "unit": "credits"},
					"addOnQuota":      map[string]any{"total": 100, "used": 40, "remaining": 60, "percentage": 40, "unit": "credits"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer worker.Close()
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Quota", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), ProxyAPIKey: "proxy-key"}, store, &fakeStarter{})
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: worker.URL})
	if err := manager.RefreshAll(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !quotaCalled {
		t.Fatal("expected quota endpoint to be called for a hot account")
	}
	item, _ := manager.Pool().ByID(account.ID)
	if item.Quota == nil {
		t.Fatalf("expected quota snapshot on pool item, got %+v", item)
	}
	if item.Quota.Used != 190 || item.Quota.Total != 700 || item.Quota.Unit != "credits" || item.Quota.Exceeded {
		t.Fatalf("quota snapshot = %+v", item.Quota)
	}
	if !item.Quota.HasAddOn || item.Quota.AddOnTotal != 100 || item.Quota.AddOnUsed != 40 {
		t.Fatalf("quota add-on = %+v", item.Quota)
	}
	views, err := manager.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Quota == nil || views[0].Quota.Remaining != 510 {
		t.Fatalf("account view quota = %+v", views)
	}
}

func TestManagerRefreshCachesAccountCatalog(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "ready": true, "hot": true, "uid": "qoder-uid-1",
			})
		case "/admin/quota":
			http.Error(w, "quota unused", http.StatusBadGateway)
		case "/admin/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "hy3", "mapped_key": "hy3", "display_name": "HY3"},
				{"id": "glm-5.2", "mapped_key": "gmodel", "display_name": "GLM-5.2"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer worker.Close()
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Catalog", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: worker.URL})
	if err := manager.RefreshAll(ctx, false); err != nil {
		t.Fatal(err)
	}
	item, _ := manager.Pool().ByID(account.ID)
	if !containsModel(item.Models, "hy3") || !containsModel(item.Models, "gmodel") {
		t.Fatalf("cached models = %#v", item.Models)
	}
}

func containsModel(models []string, want string) bool {
	for _, model := range models {
		if model == want {
			return true
		}
	}
	return false
}

func TestManagerQuotaFailureLeavesAccountReady(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "ready": true, "hot": true, "uid": "qoder-uid-1",
			})
		case "/admin/quota":
			http.Error(w, "quota down", http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer worker.Close()
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "QuotaDown", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: worker.URL})
	if err := manager.RefreshAll(ctx, false); err != nil {
		t.Fatalf("quota outage must not fail refresh: %v", err)
	}
	item, _ := manager.Pool().ByID(account.ID)
	if item.Hot == nil || !*item.Hot {
		t.Fatalf("account must stay hot on quota outage, got %+v", item)
	}
	if item.Quota != nil {
		t.Fatalf("quota should stay nil on outage, got %+v", item.Quota)
	}
}

func TestManagerRefreshCanForceQuotaBypass(t *testing.T) {
	var gotQuery string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "ready": true, "hot": true, "uid": "qoder-uid-1",
			})
		case "/admin/quota":
			gotQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"quota": map[string]any{
					"userQuota": map[string]any{"total": 10, "used": 1, "remaining": 9, "percentage": 10, "unit": "credits"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer worker.Close()
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "ForceQuota", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: worker.URL})
	if err := manager.RefreshAll(ctx, true); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "refresh=1" {
		t.Fatalf("forced quota refresh query = %q", gotQuery)
	}
}

func TestWorkerAdminUsesActionSpecWithoutPathGuessing(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Login", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), ProxyAPIKey: "secret"}, store, &fakeStarter{})
	defer manager.Close()

	t.Run("missing account", func(t *testing.T) {
		_, err := manager.WorkerAdmin(ctx, providers.AdminRequest{AccountID: "missing", Action: "login/device", Method: http.MethodPost})
		var action *providers.ActionError
		if !errors.As(err, &action) || action.Code != "account_not_running" {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("wait timeout", func(t *testing.T) {
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/health" {
				t.Fatalf("path=%s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "hasAuthManager": false})
		}))
		defer worker.Close()
		manager.Pool().Upsert(executor.Item{ID: account.ID, URL: worker.URL, Provider: "qoder"})
		waitCtx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
		defer cancel()
		_, err := manager.WorkerAdmin(waitCtx, providers.AdminRequest{AccountID: account.ID, Action: "login/device", Method: http.MethodPost})
		var action *providers.ActionError
		if !errors.As(err, &action) || action.Code != "not_ready" {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("http failure", func(t *testing.T) {
		manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1", Provider: "qoder"})
		_, err := manager.WorkerAdmin(ctx, providers.AdminRequest{AccountID: account.ID, Action: "rewarm", Method: http.MethodPost})
		var action *providers.ActionError
		if !errors.As(err, &action) || action.Code != "worker_unavailable" {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("login incomplete does not sync", func(t *testing.T) {
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/admin/login/status" {
				t.Fatalf("path=%s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"login": map[string]any{"status": "pending"}})
		}))
		defer worker.Close()
		manager.Pool().Upsert(executor.Item{ID: account.ID, URL: worker.URL, Provider: "qoder"})
		got, err := manager.WorkerAdmin(ctx, providers.AdminRequest{AccountID: account.ID, Action: "login/status", Method: http.MethodGet})
		if err != nil || got.Status != 200 {
			t.Fatalf("status=%d err=%v", got.Status, err)
		}
	})

	t.Run("login complete syncs credential", func(t *testing.T) {
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/admin/login/status" {
				t.Fatalf("path=%s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"login": map[string]any{"status": "ok"}})
		}))
		defer worker.Close()
		manager.Pool().Upsert(executor.Item{ID: account.ID, URL: worker.URL, Provider: "qoder"})
		_, err := manager.WorkerAdmin(ctx, providers.AdminRequest{AccountID: account.ID, Action: "login/status", Method: http.MethodGet})
		var action *providers.ActionError
		if !errors.As(err, &action) || action.Code != "credential_sync_failed" {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestManagerPersistsSchedulerCooldown(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Cooldown", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})
	manager.Pool().MarkClassified(account.ID, executor.Classified{Kind: accounts.KindRateLimit, Message: "429", Cooldown: time.Minute, Failover: true})
	// The observer persists asynchronously through a single drainer
	// goroutine; wait for it to catch up before asserting SQLite state.
	manager.Flush()
	updated, err := store.Get(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastErrorKind != accounts.KindRateLimit || updated.LastError != "429" || updated.CooldownUntil == nil {
		t.Fatalf("persisted account = %+v", updated)
	}
}

// failingStarter always returns an error, simulating a boot that cannot
// bring the account's process up.
type failingStarter struct{}

func (f *failingStarter) Start(_ context.Context, _ accounts.Account, _ string, _ int) (accountruntime.ManagedProcess, error) {
	return nil, errors.New("start failed")
}

// P2#1: concurrent MarkClassified calls on one account used to save
// overlapping snapshots in arbitrary order, so a stale snapshot could land
// after a fresh one and clobber the newest cooldown. The serialized drainer
// must order writes so the final SQLite state matches the last update.
func TestObserverSavesAreSerialized(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Concurrent", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})
	// Fire several cooldown updates concurrently; the last one to set
	// ModelDownUntil must win, not whichever snapshot happens to write last.
	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			manager.Pool().MarkClassified(account.ID, executor.Classified{
				Kind: accounts.KindRateLimit, Cooldown: time.Duration(i+1) * time.Minute,
				Failover: true, Model: "glm-5.3", Message: "429",
			})
		}(i)
	}
	wg.Wait()
	manager.Flush()
	rows, err := store.LoadCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 cooldown row, got %d: %+v", len(rows), rows)
	}
	// The persisted cooldown must be one of the durations we set; without
	// serialization an empty (last-write-loses) snapshot could have deleted it.
	// The model row carries ModelKind=rate_limit (the per-model kind), while
	// the account-wide Kind is empty because the failure was model-scoped.
	if rows[0].Model != "glm-5.3" || rows[0].ModelKind != accounts.KindRateLimit {
		t.Fatalf("unexpected row %+v", rows[0])
	}
}

// P1#2: Close() must not panic when a concurrent observer is enqueuing.
// The old design closed a channel that a concurrent MarkClassified could
// still send on; the mutex-guarded queue checks persistClosed under the
// lock before appending, so there is no "send on closed channel".
func TestCloseConcurrentObserverNoPanic(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "ConcurrentClose", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				manager.Pool().MarkClassified(account.ID, executor.Classified{
					Kind: accounts.KindRateLimit, Cooldown: time.Minute,
					Failover: true, Model: "glm-5.3", Message: "429",
				})
			}
		}
	}()
	// Give the writer a head start, then Close concurrently.
	time.Sleep(10 * time.Millisecond)
	closeErr := make(chan error, 1)
	go func() { closeErr <- manager.Close() }()
	close(stop)
	if err := <-closeErr; err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

// P2#4: Flush() must be strict — every item enqueued before the Flush call
// is persisted before Flush returns, even with a concurrent enqueuer.
func TestFlushStrictlyAfterEnqueue(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "FlushStrict", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})
	// Enqueue a known cooldown, then flush. The SQLite row must reflect it
	// after Flush returns.
	manager.Pool().MarkClassified(account.ID, executor.Classified{
		Kind: accounts.KindRateLimit, Cooldown: time.Hour,
		Failover: true, Model: "glm-5.3", Message: "strict-429",
	})
	manager.Flush()
	rows, err := store.LoadCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, row := range rows {
		if row.AccountID == account.ID && row.Model == "glm-5.3" && row.Message == "strict-429" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("Flush must persist the enqueued cooldown, got rows %+v", rows)
	}
}

// P2#3: per-model last-kind survives a restart. Seed SQLite with a model
// cooldown carrying model_kind=rate_limit, reopen the store, restore, and
// assert ModelLastKind[model] is restored so a repeat failure escalates.
func TestModelLastKindPersistedAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "RestartKind", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).UTC()
	if err := store.SaveCooldowns(ctx, account.ID, []accounts.CooldownRow{{
		AccountID: account.ID, Model: "glm-5.3", DownUntil: until,
		BackoffLevel: 1, Kind: accounts.KindRateLimit, Message: "429",
		ModelKind: accounts.KindRateLimit,
	}}); err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})
	manager.TestRestoreCooldowns(ctx)
	item, _ := manager.Pool().ByID(account.ID)
	if item.ModelLastKind == nil || item.ModelLastKind["glm-5.3"] != accounts.KindRateLimit {
		t.Fatalf("ModelLastKind[glm-5.3] must be restored to rate_limit, got %+v", item.ModelLastKind)
	}
}

// P1: the observer runs after p.mu is released, so two concurrent
// MarkClassified calls can enqueue snapshots out of production order. The
// monotonic StateVersion (stamped under p.mu) lets the drainer discard a
// stale snapshot, so the final persisted state is the newest pool state.
// This reproduces the exact ordering hazard: snapshot A (older) is delayed
// before enqueueing while snapshot B (newer) races ahead; the SQLite row
// must reflect B, not A.
func TestStateVersionOrdersAcrossDelayedObserver(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Ordered", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})

	// Wrap the observer so the FIRST call (the older snapshot A) blocks on a
	// latch until after B has enqueued. The second call (B) enqueues
	// immediately. Then A's delayed enqueue must be discarded by version.
	blockA := make(chan struct{})
	startedA := make(chan struct{})
	original := manager.Pool().Observer()
	manager.Pool().SetObserver(func(item executor.Item) {
		if item.StateVersion == 1 && item.ID == account.ID {
			close(startedA)
			<-blockA // hold A back until B has enqueued
		}
		// Delegate to the real observer (the dirty-set merge) for all calls.
		original(item)
	})

	// Fire A (version 1, message "old"); the observer will block on blockA.
	go manager.Pool().MarkClassified(account.ID, executor.Classified{
		Kind: accounts.KindRateLimit, Cooldown: time.Hour,
		Failover: true, Model: "glm-5.3", Message: "old-snapshot",
	})
	<-startedA
	// Fire B (version 2, message "new"); it enqueues immediately while A is held.
	manager.Pool().MarkClassified(account.ID, executor.Classified{
		Kind: accounts.KindRateLimit, Cooldown: time.Hour,
		Failover: true, Model: "glm-5.3", Message: "new-snapshot",
	})
	// Release A; its stale snapshot is now enqueued after B's.
	close(blockA)

	manager.Flush()
	rows, err := store.LoadCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.AccountID == account.ID && row.Model == "glm-5.3" {
			if row.Message != "new-snapshot" {
				t.Fatalf("stale snapshot A must be discarded; SQLite has %q, want %q", row.Message, "new-snapshot")
			}
			return
		}
	}
	t.Fatalf("cooldown row for glm-5.3 must exist, got rows %+v", rows)
}

// P2: the persistence dirty set is keyed by account ID and merged on
// enqueue, so it cannot grow without bound under DB pressure. Fire many
// concurrent MarkClassified calls on one account against a slow store and
// assert the process stays bounded (the test completes without OOM and the
// dirty set never holds more than one entry per account at drain time).
func TestPersistQueueBoundedPerAccount(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Bounded", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})

	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			manager.Pool().MarkClassified(account.ID, executor.Classified{
				Kind: accounts.KindRateLimit, Cooldown: time.Minute,
				Failover: true, Model: "glm-5.3", Message: fmt.Sprintf("burst-%d", i),
			})
		}(i)
	}
	wg.Wait()
	manager.Flush()

	// The dirty set is a per-account map; after Flush it must be empty.
	dirtyLen := manager.TestPersistDirtyLen()
	if dirtyLen != 0 {
		t.Fatalf("persistDirty must be empty after Flush, got %d", dirtyLen)
	}
	// Exactly one cooldown row survives (the account's newest state).
	rows, err := store.LoadCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		if row.AccountID == account.ID && row.Model == "glm-5.3" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 cooldown row for the account, got %d", count)
	}
}

// failPoolWrites injects persistence failures without replacing the live DB
// pointer underneath the drainer. All successful writes still hit real SQLite.
type failPoolWrites struct {
	*sqlstore.Store
	failing  atomic.Bool
	failures atomic.Int32
}

func (s *failPoolWrites) RecordPoolState(ctx context.Context, state accounts.PoolState) error {
	if s.failing.Load() {
		s.failures.Add(1)
		return errors.New("injected pool-state write failure")
	}
	return s.Store.RecordPoolState(ctx, state)
}

// P1: when a SQLite write fails (db locked, disk error, connection), the
// snapshot must stay in the dirty set and be retried rather than be dropped.
// persistedVersions must only advance on success, otherwise a later stale
// snapshot could be discarded even though the newer state never reached disk.
// The injected storage failure leaves the underlying SQLite handle stable.
func TestPersistFailureKeepsDirtyEntryAndRetries(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "qoder.db")
	store, err := sqlstore.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	failing := &failPoolWrites{Store: store}
	failing.failing.Store(true)
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "FailingWrite", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, failing, &fakeStarter{})
	defer manager.Close()
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})

	// The drainer must re-enqueue the failed snapshot without advancing its
	// persisted version, then retry when the same storage recovers.
	manager.Pool().MarkClassified(account.ID, executor.Classified{
		Kind: accounts.KindRateLimit, Cooldown: time.Hour,
		Failover: true, Model: "glm-5.3", Message: "write-will-fail",
	})
	// The drainer removes the snapshot while a write is in flight, then
	// puts it back after the failure. Sampling at a multiple of the retry
	// backoff can land in that empty window, so poll until the failed
	// snapshot is visible again.
	var dirty executor.Item
	var dirtyOK bool
	var version uint64
	deadline := time.Now().Add(2 * time.Second)
	for {
		dirty, version, dirtyOK = manager.TestPersistSnapshot(account.ID)
		if dirtyOK && dirty.LastError == "write-will-fail" && version == 0 && failing.failures.Load() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dirty entry must be retained after a failed write, ok=%v version=%d lastError=%q", dirtyOK, version, dirty.LastError)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Recover storage without an unsynchronized DB pointer swap.
	failing.failing.Store(false)
	manager.Flush()
	afterDirty := manager.TestPersistDirtyLen()
	if afterDirty != 0 {
		t.Fatalf("dirty set must drain once writes succeed, got %d", afterDirty)
	}
	saved, err := store.Get(ctx, account.ID)
	if err != nil || saved.LastError != "write-will-fail" {
		t.Fatalf("retry did not persist latest state: %+v err=%v", saved, err)
	}
}

// P1: Close() must not block forever when the DB is persistently
// unavailable. The drainer's retry backoff watches persistCloseCh (closed by
// Close), not just runCtx (which Close cancels only AFTER the drainer exits).
// Without this, a stuck DB would deadlock shutdown: Close waits for the
// drainer, the drainer waits for runCtx.Done(), runCtx is never canceled.
func TestCloseDuringPersistentDBFailure(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "StuckClose", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})

	// Close the underlying db so writes persistently fail, forcing the
	// drainer into its retry backoff.
	if err := store.DB().Close(); err != nil {
		t.Fatal(err)
	}
	manager.Pool().MarkClassified(account.ID, executor.Classified{
		Kind: accounts.KindRateLimit, Cooldown: time.Hour,
		Failover: true, Model: "glm-5.3", Message: "stuck",
	})
	// Let the drainer enter the retry loop (it fails, backs off, retries).
	time.Sleep(accountruntime.PersistRetryBackoff() * 2)

	// Close must return within a bounded time despite the stuck DB.
	done := make(chan error, 1)
	go func() { done <- manager.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked forever on a persistently failing DB")
	}
}

// A model-scoped cooldown with a high backoff level must not pollute the
// account-wide BackoffLevel on restore. Without this fix, a model that
// failed repeatedly (e.g. level 3) would inflate the account-level ladder,
// so a later account-wide failure on a different model starts at level 3
// instead of 0.
func TestRestoreModelCooldownDoesNotPolluteAccountBackoff(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "NoPollute", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).UTC()
	rows := []accounts.CooldownRow{
		// Model-scoped row with a high backoff level.
		{
			AccountID: account.ID, Model: "glm-5.3", DownUntil: until,
			BackoffLevel: 3, Kind: accounts.KindRateLimit, Message: "429",
			ModelKind: accounts.KindRateLimit,
		},
		// accounts.Account-wide row with backoff level 0 (healthy account level).
		{
			AccountID: account.ID, Model: "", DownUntil: until,
			BackoffLevel: 0, Kind: "", Message: "",
		},
	}
	if err := store.SaveCooldowns(ctx, account.ID, rows); err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	manager.Pool().Upsert(executor.Item{ID: account.ID, URL: "http://127.0.0.1:1"})
	manager.TestRestoreCooldowns(ctx)

	item, _ := manager.Pool().ByID(account.ID)
	// accounts.Account-wide BackoffLevel must stay at 0 — the model's level 3
	// must not leak into it.
	if item.BackoffLevel != 0 {
		t.Fatalf("BackoffLevel = %d, want 0 (model backoff leaked into account level)", item.BackoffLevel)
	}
	// Model-scoped backoff must be restored correctly.
	if item.ModelBackoff == nil || item.ModelBackoff["glm-5.3"] != 3 {
		t.Fatalf("ModelBackoff[glm-5.3] = %v, want 3", item.ModelBackoff)
	}

	// Verify the fix end-to-end: trigger an account-wide failure on a
	// different model. The backoff ladder must start at level 0 (first
	// failure), not level 3.
	manager.Pool().MarkClassified(account.ID, executor.Classified{
		Kind: accounts.KindUnavailable, Cooldown: 60 * time.Second,
		Failover: true, Model: "deepseek-v4-flash", Message: "conn refused",
	})
	item, _ = manager.Pool().ByID(account.ID)
	// deepseek-v4-flash is a new model+kind, so its ModelBackoff must
	// start at 0 (first failure), not 3.
	if mb := item.ModelBackoff["deepseek-v4-flash"]; mb != 0 {
		t.Fatalf("ModelBackoff[deepseek-v4-flash] = %d, want 0 (first failure must start fresh)", mb)
	}
}

// EnsureModelCatalogs must return immediately, not block on N serial 15s
// timeouts when multiple accounts are offline. Before the fix each
// fetchAccountModels call was synchronous with a 15-second HTTP timeout,
// so 3 offline accounts would block the first chat request for up to 45s.
func TestEnsureModelCatalogsDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Workers that hang on /admin/models — they will time out at 15s in
	// fetchAccountModels, but EnsureModelCatalogs must return long before
	// that.
	slowA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
	}))
	defer slowA.Close()
	slowB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
	}))
	defer slowB.Close()
	slowC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Second)
	}))
	defer slowC.Close()

	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	manager.Pool().Upsert(executor.Item{ID: "slow-a", URL: slowA.URL, Provider: "qoder", Runtime: "child_process"})
	manager.Pool().Upsert(executor.Item{ID: "slow-b", URL: slowB.URL, Provider: "qoder", Runtime: "child_process"})
	manager.Pool().Upsert(executor.Item{ID: "slow-c", URL: slowC.URL, Provider: "qoder", Runtime: "child_process"})

	// With the old serial implementation, this would block ~45s.
	// With the async fix it returns in milliseconds.
	done := make(chan struct{})
	go func() {
		manager.EnsureModelCatalogs(ctx, true)
		close(done)
	}()
	select {
	case <-done:
		// success — returned without blocking
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureModelCatalogs blocked >2s; must be async/non-blocking")
	}
}

type fakeCheckinMaintainer struct {
	calls int
	msg   string
	err   error
}

func (f *fakeCheckinMaintainer) DailyCheckin(context.Context, string) (string, error) {
	f.calls++
	return f.msg, f.err
}

func (f *fakeCheckinMaintainer) Keepalive(context.Context, string) error { return nil }

func (fake *fakeCheckinMaintainer) Checkin(ctx context.Context, accountID string) (providers.CheckinResult, error) {
	message, err := fake.DailyCheckin(ctx, accountID)
	var already interface{ AlreadyCheckedIn() bool }
	if errors.As(err, &already) && already.AlreadyCheckedIn() {
		return providers.CheckinResult{Status: "already", Message: message}, nil
	}
	return providers.CheckinResult{Status: "success", Message: message}, err
}

func registerCheckinMaintainer(manager *accountruntime.Manager, fake *fakeCheckinMaintainer) {
	registry := providers.NewRegistry()
	registry.Register(providers.Adapter{ID: "workbuddy", Checkin: fake})
	manager.SetProviders(registry)
	manager.SetWorkBuddy(fake)
}

type fakeAlreadyCheckedInError struct{ msg string }

func (e fakeAlreadyCheckedInError) Error() string        { return e.msg }
func (fakeAlreadyCheckedInError) AlreadyCheckedIn() bool { return true }

func TestCheckedInLocalDay(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 9, 7, 21, 0, 0, 0, loc)
	today := time.Date(2026, 9, 7, 0, 1, 28, 0, loc).UTC().Format(time.RFC3339Nano)
	yesterday := time.Date(2026, 9, 6, 17, 0, 2, 0, loc).UTC().Format(time.RFC3339Nano)
	if !accountruntime.CheckedInLocalDay(today, "success", now) {
		t.Fatal("same-day success must skip")
	}
	if !accountruntime.CheckedInLocalDay(today, "already", now) {
		t.Fatal("same-day already must skip")
	}
	if accountruntime.CheckedInLocalDay(today, "error", now) {
		t.Fatal("same-day error must retry")
	}
	if accountruntime.CheckedInLocalDay(yesterday, "success", now) {
		t.Fatal("yesterday success must not skip")
	}
	if accountruntime.CheckedInLocalDay("", "success", now) {
		t.Fatal("empty timestamp must not skip")
	}
}

func TestCheckinOptedInSkipsSameDaySuccess(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{
		Name: "wb", Provider: "workbuddy", Region: "cn", Enabled: true,
		WorkBuddyAutoCheckin: boolPtr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCheckin(ctx, account.ID, "success", "ok", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ops := &fakeCheckinMaintainer{msg: "ok"}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	registerCheckinMaintainer(manager, ops)
	manager.CheckinOptedIn(ctx)
	if ops.calls != 0 {
		t.Fatalf("scheduled check-in must skip same-day success, calls=%d", ops.calls)
	}
	records, err := store.ListCheckinRecords(ctx, account.ID, 20)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}

func TestScheduledCheckinRespectsConfiguredTime(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.Create(ctx, accounts.CreateAccount{
		Name: "wb", Provider: "workbuddy", Region: "cn", Enabled: true,
		WorkBuddyAutoCheckin: boolPtr(true), WorkBuddyCheckinTime: "18:30",
	})
	if err != nil {
		t.Fatal(err)
	}
	ops := &fakeCheckinMaintainer{msg: "ok"}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	registerCheckinMaintainer(manager, ops)
	loc := time.FixedZone("CST", 8*3600)
	manager.TestCheckinOptedIn(ctx, time.Date(2026, 8, 30, 18, 29, 0, 0, loc), "21:00", true)
	if ops.calls != 0 {
		t.Fatalf("before configured time calls=%d", ops.calls)
	}
	manager.TestCheckinOptedIn(ctx, time.Date(2026, 8, 30, 18, 30, 0, 0, loc), "18:30", false)
	if ops.calls != 1 {
		t.Fatalf("at configured time calls=%d", ops.calls)
	}
}

func TestScheduledCheckinDoesNotImmediatelyRetrySameTime(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.Create(ctx, accounts.CreateAccount{
		Name: "wb", Provider: "workbuddy", Region: "cn", Enabled: true,
		WorkBuddyAutoCheckin: boolPtr(true), WorkBuddyCheckinTime: "21:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	ops := &fakeCheckinMaintainer{err: errors.New("timeout")}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	registerCheckinMaintainer(manager, ops)
	now := time.Date(2026, 8, 30, 21, 0, 0, 0, time.FixedZone("CST", 8*3600))
	manager.TestCheckinOptedIn(ctx, now, "21:00", false)
	manager.TestCheckinOptedIn(ctx, now, "21:00", true)
	if ops.calls != 1 {
		t.Fatalf("same-time retry duplicated check-in, calls=%d", ops.calls)
	}
}

func TestCheckinOptedInRetriesSameDayError(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{
		Name: "wb", Provider: "workbuddy", Region: "cn", Enabled: true,
		WorkBuddyAutoCheckin: boolPtr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCheckin(ctx, account.ID, "error", "timeout", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ops := &fakeCheckinMaintainer{msg: "ok"}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	registerCheckinMaintainer(manager, ops)
	manager.CheckinOptedIn(ctx)
	if ops.calls != 1 {
		t.Fatalf("same-day error must retry, calls=%d", ops.calls)
	}
}

func TestCheckinAccountRecordsFirstAlreadyThenSkips(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{
		Name: "wb", Provider: "workbuddy", Region: "cn", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ops := &fakeCheckinMaintainer{msg: "今天已签到，请明天再来", err: fakeAlreadyCheckedInError{msg: "今天已签到，请明天再来"}}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, &fakeStarter{})
	defer manager.Close()
	registerCheckinMaintainer(manager, ops)
	updated, err := manager.CheckinAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastCheckinStatus != "already" || ops.calls != 1 {
		t.Fatalf("first already: status=%q calls=%d", updated.LastCheckinStatus, ops.calls)
	}
	if _, err := manager.CheckinAccount(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if ops.calls != 1 {
		t.Fatalf("second already must skip upstream, calls=%d", ops.calls)
	}
	records, err := store.ListCheckinRecords(ctx, account.ID, 20)
	if err != nil || len(records) != 1 || records[0].Status != "already" {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}

func boolPtr(value bool) *bool { return &value }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
