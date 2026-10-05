//go:build qoder_worker

// Worker-lifecycle tests: they assert the child-process behavior (spawn,
// restart, credential materialization, admin endpoints) that native-path
// accounts no longer exercise. Run locally during the migration with:
//
//	go test -tags qoder_worker ./internal/runtime/
package runtime_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/executor"
	accountruntime "github.com/caigee-cmd/cli2api/internal/runtime"
	sqlstore "github.com/caigee-cmd/cli2api/internal/store"
)

func TestManagerRetriesInitialStartFailure(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "RetryBoot", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{failures: 1, started: make(chan *fakeProcess, 1)}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), RestartDelay: time.Millisecond}, store, starter)
	defer manager.Close()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Pool().Pick("", nil); ok {
		t.Fatal("failed account must not be routable during recovery")
	}
	select {
	case <-starter.started:
	case <-time.After(time.Second):
		t.Fatal("initial start failure was not recovered")
	}
	deadline := time.Now().Add(time.Second)
	var item executor.Item
	var ok bool
	for time.Now().Before(deadline) {
		item, ok = manager.Pool().ByID(account.ID)
		if ok && item.Restarts == 1 && item.RuntimeState == "starting" && item.Ready != nil && !*item.Ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ok {
		t.Fatal("account disappeared during recovery")
	}
	if item.Restarts != 1 || item.RuntimeState != "starting" || item.Ready == nil || *item.Ready {
		t.Fatalf("recovered runtime state = %+v", item)
	}
}

func TestManagerDisabledAccountDoesNotRestartAfterStartFailure(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "DisableRetry", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{failures: 100}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), RestartDelay: 50 * time.Millisecond}, store, starter)
	defer manager.Close()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(ctx, account.ID, accounts.UpdateAccount{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	attempts := len(starter.accounts)
	time.Sleep(100 * time.Millisecond)
	if got := len(starter.accounts); got != attempts {
		t.Fatalf("disabled account was restarted: attempts %d -> %d", attempts, got)
	}
}

func TestManagerReenableDuringRecoveryDoesNotGetMarkedStarting(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "ReenableRetry", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{failures: 1}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), RestartDelay: 100 * time.Millisecond}, store, starter)
	defer manager.Close()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	foundDead := false
	for time.Now().Before(deadline) {
		item, ok := manager.Pool().ByID(account.ID)
		if ok && item.RuntimeState == "dead" {
			foundDead = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !foundDead {
		t.Fatal("account did not enter recovery")
	}
	if err := manager.Update(ctx, account.ID, accounts.UpdateAccount{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Update(ctx, account.ID, accounts.UpdateAccount{Enabled: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Duration(100*time.Millisecond))
	item, ok := manager.Pool().ByID(account.ID)
	if !ok || item.RuntimeState != "starting" || item.Ready == nil || *item.Ready {
		t.Fatalf("reenabled account was clobbered by stale recovery: %+v ok=%v", item, ok)
	}
}

func TestManagerStartsEnabledAccountsAndMaterializesCredentials(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlstore.OpenStore(filepath.Join(dataDir, "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Work", Enabled: true, MaxInFlight: 3})
	if err != nil {
		t.Fatal(err)
	}
	credential := accounts.NativeCredential{UserBlob: []byte("ciphertext"), MachineID: "machine-1"}
	if err := store.SaveCredential(ctx, account.ID, "native", credential); err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: dataDir, BasePort: 32100}, store, starter)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	if len(starter.accounts) != 1 || starter.accounts[0].ID != account.ID {
		t.Fatalf("started accounts = %+v", starter.accounts)
	}
	home := starter.homes[0]
	userBlob, err := os.ReadFile(filepath.Join(home, ".qoder", ".auth", "user"))
	if err != nil {
		t.Fatal(err)
	}
	machineID, err := os.ReadFile(filepath.Join(home, ".qoder", ".auth", "machine_id"))
	if err != nil {
		t.Fatal(err)
	}
	if string(userBlob) != "ciphertext" || string(machineID) != "machine-1" {
		t.Fatalf("materialized user=%q machine=%q", userBlob, machineID)
	}
	item, ok := manager.Pool().ByID(account.ID)
	if !ok || item.URL != "http://127.0.0.1:32100" || item.RuntimeState != "starting" {
		t.Fatalf("started item = %+v ok=%v", item, ok)
	}
	if _, ok := manager.Pool().Pick("", nil); ok {
		t.Fatal("starting account must not be routable before health succeeds")
	}
	manager.Pool().MergeHealth(account.ID, true, false, 0, 0, "")
	picked, ok := manager.Pool().Pick("", nil)
	if !ok || picked.ID != account.ID || picked.URL != "http://127.0.0.1:32100" {
		t.Fatalf("picked = %+v ok=%v", picked, ok)
	}
}

func TestManagerCreatesDisablesAndDeletesAccount(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlstore.OpenStore(filepath.Join(dataDir, "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	starter := &fakeStarter{}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: dataDir, BasePort: 32200}, store, starter)

	account, err := manager.Create(ctx, accounts.CreateAccount{Name: "New", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Pool().ByID(account.ID); !ok {
		t.Fatal("created account was not added to pool")
	}
	process := manager.TestProcess(account.ID).(*fakeProcess)
	if err := manager.Update(ctx, account.ID, accounts.UpdateAccount{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if !process.stopped {
		t.Fatal("disabled account process was not stopped")
	}
	if _, ok := manager.Pool().ByID(account.ID); ok {
		t.Fatal("disabled account remained in pool")
	}
	if err := manager.Delete(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, account.ID); !errors.Is(err, accounts.ErrAccountNotFound) {
		t.Fatalf("deleted account error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "runtime", account.ID)); !os.IsNotExist(err) {
		t.Fatalf("runtime directory still exists: %v", err)
	}
}

func TestManagerSyncsCredentialWrittenByQoderCLI(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlstore.OpenStore(filepath.Join(dataDir, "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	starter := &fakeStarter{}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: dataDir}, store, starter)
	account, err := manager.Create(ctx, accounts.CreateAccount{Name: "OAuth", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Join(starter.homes[0], ".qoder", ".auth")
	if err := os.WriteFile(filepath.Join(authDir, "user"), []byte("new-oauth-blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "machine_id"), []byte("machine-oauth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.SyncCredential(ctx, account.ID, "oauth"); err != nil {
		t.Fatal(err)
	}
	credential, err := store.LoadCredential(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(credential.UserBlob) != "new-oauth-blob" || credential.MachineID != "machine-oauth" {
		t.Fatalf("synced credential = %+v", credential)
	}
}

func TestQoderRuntimeSpecSelectsCNCLIAndConfigDir(t *testing.T) {
	cfg := accountruntime.ManagerConfig{
		QoderCLIPath:   "/opt/qodercli.js",
		QoderCNCLIPath: "/opt/qoderclicn.js",
	}
	globalPath, globalSite, globalDir, globalEnv, err := accountruntime.QoderRuntimeSpec(cfg, accounts.Account{ProviderRegion: "global"}, "/run/acc-g")
	if err != nil {
		t.Fatal(err)
	}
	if globalPath != "/opt/qodercli.js" || globalSite != "global" || globalDir != "/run/acc-g/.qoder" || globalEnv != "QODER_CONFIG_DIR" {
		t.Fatalf("global spec path=%s site=%s dir=%s env=%s", globalPath, globalSite, globalDir, globalEnv)
	}
	cnPath, cnSite, cnDir, cnEnv, err := accountruntime.QoderRuntimeSpec(cfg, accounts.Account{ProviderRegion: "cn"}, "/run/acc-c")
	if err != nil {
		t.Fatal(err)
	}
	if cnPath != "/opt/qoderclicn.js" || cnSite != "cn" || cnDir != "/run/acc-c/.qoder-cn" || cnEnv != "QODERCN_CONFIG_DIR" {
		t.Fatalf("cn spec path=%s site=%s dir=%s env=%s", cnPath, cnSite, cnDir, cnEnv)
	}
	if _, _, _, _, err := accountruntime.QoderRuntimeSpec(accountruntime.ManagerConfig{QoderCLIPath: "/opt/qodercli.js"}, accounts.Account{ProviderRegion: "cn"}, "/run/acc-c"); err == nil {
		t.Fatal("expected missing CN CLI path to fail")
	}
}

func TestManagerMaterializesAndSyncsQoderCNCredentials(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlstore.OpenStore(filepath.Join(dataDir, "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "CN", Provider: "qoder", Region: "cn", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCredential(ctx, account.ID, "native", accounts.NativeCredential{UserBlob: []byte("cn-blob"), MachineID: "cn-machine"}); err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: dataDir, BasePort: 32400}, store, starter)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(starter.homes) != 1 {
		t.Fatalf("homes = %v", starter.homes)
	}
	authDir := filepath.Join(starter.homes[0], ".qoder-cn", ".auth")
	if _, err := os.Stat(filepath.Join(starter.homes[0], ".qoder", ".auth", "user")); !os.IsNotExist(err) {
		t.Fatal("CN account must not materialize global .qoder credentials")
	}
	if err := os.WriteFile(filepath.Join(authDir, "user"), []byte("cn-oauth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "machine_id"), []byte("cn-mid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.SyncCredential(ctx, account.ID, "oauth"); err != nil {
		t.Fatal(err)
	}
	credential, err := store.LoadCredential(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(credential.UserBlob) != "cn-oauth" || credential.MachineID != "cn-mid" {
		t.Fatalf("synced CN credential = %+v", credential)
	}
}

func TestManagerDeleteDuringRestartDoesNotLeaveDuplicateRecovery(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlstore.OpenStore(filepath.Join(dataDir, "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Crash", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{started: make(chan *fakeProcess, 4)}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: dataDir, RestartDelay: 30 * time.Millisecond, RestartMaxDelay: 30 * time.Millisecond}, store, starter)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	first := <-starter.started
	first.done <- errors.New("crashed")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		recovering := manager.TestRecovering(account.ID)
		if recovering {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := manager.Delete(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := store.Get(ctx, account.ID); !errors.Is(err, accounts.ErrAccountNotFound) {
		t.Fatalf("deleted account still in store: %v", err)
	}
	if _, ok := manager.Pool().ByID(account.ID); ok {
		t.Fatal("deleted account remained in pool")
	}
	processCount := manager.TestProcessCount()
	recovering := manager.TestRecovering(account.ID)
	if processCount != 0 {
		t.Fatalf("leftover processes=%d", processCount)
	}
	if recovering {
		// Delete does not clear recovering[]; the goroutine exits after the
		// next store lookup sees accounts.ErrAccountNotFound. Assert it does not spawn
		// another process while that flag is still set.
		select {
		case extra := <-starter.started:
			t.Fatalf("delete during recovery started another process: %+v", extra)
		case <-time.After(80 * time.Millisecond):
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "runtime", account.ID)); !os.IsNotExist(err) {
		t.Fatalf("runtime dir leftover: %v", err)
	}
}

func TestManagerRestartsUnexpectedlyExitedEnabledAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.Create(ctx, accounts.CreateAccount{Name: "Restart", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{started: make(chan *fakeProcess, 2)}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), RestartDelay: time.Millisecond}, store, starter)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	first := <-starter.started
	first.done <- errors.New("crashed")
	select {
	case second := <-starter.started:
		if second == first {
			t.Fatal("manager reused exited process")
		}
		item, ok := manager.Pool().ByID(starter.accounts[0].ID)
		if !ok || item.Restarts != 1 {
			t.Fatalf("restart state = %+v ok=%v", item, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("account process was not restarted")
	}
}

func TestCloseStopsRecoveryBeforeNewDaemonEscapes(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "CloseRace", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	starter := &delayedStarter{started: make(chan *fakeProcess, 2), delayAfter: 1, delay: 80 * time.Millisecond}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), RestartDelay: time.Millisecond}, store, starter)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	first := <-starter.started
	first.done <- errors.New("crashed")

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if starter.startCount() >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if starter.startCount() < 2 {
		t.Fatal("recovery did not attempt a restart before Close")
	}

	done := make(chan error, 1)
	go func() { done <- manager.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked while a recovery start was in flight")
	}

	for _, process := range starter.processes() {
		if !process.stopped && process != first {
			t.Fatal("recovery process was left running after Close")
		}
	}
	if got := starter.startCount(); got != 2 {
		t.Fatalf("starts after Close = %d, want 2", got)
	}
}

func TestManagerEscalatesConsecutiveRestartBackoffSeparately(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "Backoff", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{started: make(chan *fakeProcess, 3)}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), RestartDelay: time.Millisecond}, store, starter)
	defer manager.Close()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	first := <-starter.started
	first.done <- errors.New("first crash")
	second := <-starter.started
	second.done <- errors.New("second crash")
	third := <-starter.started
	if third == second {
		t.Fatal("manager reused exited process")
	}
	// fakeStarter announces the process before Start returns. Wait until the
	// manager has registered that process and copied the restart count into the
	// pool instead of racing the remainder of startAccount.
	deadline := time.Now().Add(time.Second)
	var item executor.Item
	var ok bool
	for time.Now().Before(deadline) {
		item, ok = manager.Pool().ByID(account.ID)
		if ok && item.Restarts == 2 && item.RestartBackoffLevel == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !ok {
		t.Fatal("account disappeared during consecutive recovery")
	}
	if item.Restarts != 2 || item.RestartBackoffLevel != 2 {
		t.Fatalf("restart count/backoff = %d/%d, want 2/2", item.Restarts, item.RestartBackoffLevel)
	}
}

func TestManagerRestartDelayIsBounded(t *testing.T) {
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{RestartDelay: 2 * time.Second, RestartMaxDelay: 5 * time.Second}, nil, &fakeStarter{})
	if got := manager.TestRestartDelay(1); got != 2*time.Second {
		t.Fatalf("level 1 delay = %v", got)
	}
	if got := manager.TestRestartDelay(2); got != 4*time.Second {
		t.Fatalf("level 2 delay = %v", got)
	}
	if got := manager.TestRestartDelay(3); got != 5*time.Second {
		t.Fatalf("level 3 delay = %v", got)
	}
}

func TestCloseStopsRunningChildren(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "CloseStop", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{started: make(chan *fakeProcess, 1)}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, starter)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	first := <-starter.started
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if !first.stopped {
		t.Fatal("Close must stop running children after persist drain")
	}
	if manager.TestProcessCount() != 0 {
		t.Fatalf("processes leftover=%d", manager.TestProcessCount())
	}
}

func TestCloseCancelsRecoveryWithoutRespawn(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{Name: "CloseCancel", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	starter := &fakeStarter{started: make(chan *fakeProcess, 2)}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{
		DataDir: t.TempDir(), RestartDelay: time.Hour, RestartMaxDelay: time.Hour,
	}, store, starter)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	first := <-starter.started
	first.done <- errors.New("crashed")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !manager.TestRecovering(account.ID) {
		time.Sleep(5 * time.Millisecond)
	}
	if !manager.TestRecovering(account.ID) {
		t.Fatal("expected recovery goroutine after crash")
	}
	done := make(chan error, 1)
	go func() { done <- manager.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after canceling recovery")
	}
	if manager.TestRecovering(account.ID) {
		t.Fatal("recovery flag must clear after Close waits recoverDone")
	}
	select {
	case extra := <-starter.started:
		t.Fatalf("close must not spawn another process: %+v", extra)
	default:
	}
}

// P2#2: a boot-time startAccount failure used to call MarkDown(0), which
// triggered the observer's SaveCooldowns with an empty in-memory cooldown
// set and DELETEd any persisted cooldowns for the account. The failure path
// now records the error without triggering persistence, so a restart that
// fails to start a rate-limited account leaves its cooldown intact in SQLite
// and restoreCooldowns reloads it into memory.
func TestStartFailureKeepsPersistedCooldowns(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	account, err := store.Create(ctx, accounts.CreateAccount{
		Name: "FailingBoot", Enabled: true, Provider: "qoder", Region: "global",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed SQLite with a model cooldown as if a previous run recorded it.
	seedUntil := time.Now().Add(time.Hour).UTC()
	if err := store.SaveCooldowns(ctx, account.ID, []accounts.CooldownRow{{
		AccountID: account.ID, Model: "glm-5.3", DownUntil: seedUntil,
		BackoffLevel: 2, Kind: accounts.KindRateLimit, Message: "429",
	}}); err != nil {
		t.Fatal(err)
	}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), QoderCLIPath: "unused"}, store, &failingStarter{})
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer manager.Close()
	manager.Flush()
	// The cooldown must still be in SQLite, not deleted by the start failure.
	rows, err := store.LoadCooldowns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range rows {
		if row.AccountID == account.ID && row.Model == "glm-5.3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("persisted glm-5.3 cooldown must survive start failure, got rows %+v", rows)
	}
}

func TestReloadProxyURLRestartsOnlyInheritingQoder(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	inherits, err := store.Create(ctx, accounts.CreateAccount{Name: "InheritsGlobal", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	overrides, err := store.Create(ctx, accounts.CreateAccount{Name: "HasAccountProxy", Enabled: true, ProxyURL: "http://account.example:9090"})
	if err != nil {
		t.Fatal(err)
	}
	workbuddy, err := store.Create(ctx, accounts.CreateAccount{Name: "WorkBuddy", Provider: "workbuddy", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	trae, err := store.Create(ctx, accounts.CreateAccount{Name: "Trae", Provider: "trae", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	starter := &fakeStarter{}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, starter)
	defer manager.Close()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	before := len(starter.accounts)

	if err := manager.ReloadProxyURL(ctx, "http://new-global.example:8080"); err != nil {
		t.Fatalf("ReloadProxyURL: %v", err)
	}

	restarted := map[string]bool{}
	for _, account := range starter.accounts[before:] {
		restarted[account.ID] = true
	}
	if !restarted[inherits.ID] {
		t.Fatal("inheriting Qoder account was not restarted")
	}
	if restarted[overrides.ID] {
		t.Fatal("Qoder account with its own proxy was restarted")
	}
	if restarted[workbuddy.ID] {
		t.Fatal("WorkBuddy account was restarted (in-process)")
	}
	if restarted[trae.ID] {
		t.Fatal("Trae account was restarted (in-process)")
	}
}

func TestReloadProxyURLLogsAllFailuresAndContinues(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := store.Create(ctx, accounts.CreateAccount{Name: "First", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, accounts.CreateAccount{Name: "Second", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	starter := &fakeStarter{}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir()}, store, starter)
	defer manager.Close()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Force the next start attempt to fail; the reload must still attempt the
	// remaining accounts and report the failure.
	starter.failures = 1
	if err := manager.ReloadProxyURL(ctx, "http://new-global.example:8080"); err == nil {
		t.Fatal("expected a joined reload error")
	}

	attempted := map[string]bool{}
	for _, account := range starter.accounts {
		attempted[account.ID] = true
	}
	if !attempted[first.ID] || !attempted[second.ID] {
		t.Fatalf("reload did not attempt every account: %v", attempted)
	}
}

func TestReloadProxyURLSkipsUnchangedValue(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "Inherits", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	starter := &fakeStarter{}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), ProxyURL: "http://global.example:8080"}, store, starter)
	defer manager.Close()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(starter.accounts); got != 1 {
		t.Fatalf("initial starts = %d, want 1", got)
	}

	// Same value (module whitespace): no worker restart.
	if err := manager.ReloadProxyURL(ctx, "  http://global.example:8080  "); err != nil {
		t.Fatalf("ReloadProxyURL: %v", err)
	}
	if got := len(starter.accounts); got != 1 {
		t.Fatalf("worker restarted for an unchanged proxy: starts = %d, want 1", got)
	}

	// A real change still restarts.
	if err := manager.ReloadProxyURL(ctx, "http://other.example:9090"); err != nil {
		t.Fatalf("ReloadProxyURL: %v", err)
	}
	if got := len(starter.accounts); got != 2 {
		t.Fatalf("worker not restarted for a changed proxy: starts = %d, want 2", got)
	}
}

func TestReloadProxyURLRetriesAfterFailureWithSameValue(t *testing.T) {
	ctx := context.Background()
	store, err := sqlstore.OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "Inherits", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	starter := &fakeStarter{}
	manager := accountruntime.NewManager(accountruntime.ManagerConfig{DataDir: t.TempDir(), ProxyURL: "http://old.example:8080"}, store, starter)
	defer manager.Close()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(starter.accounts); got != 1 {
		t.Fatalf("initial starts = %d, want 1", got)
	}

	const newProxy = "http://new.example:9090"

	// Attempt to switch to the new proxy with an already-cancelled context:
	// the restart fails, so the reload reports an error.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := manager.ReloadProxyURL(cancelled, newProxy); err == nil {
		t.Fatal("reload with a cancelled context unexpectedly succeeded")
	}
	if got := len(starter.accounts); got != 1 {
		t.Fatalf("failed reload restarted workers: starts = %d, want 1", got)
	}

	// Resubmitting the *same* value with a healthy context must retry and
	// restart the inheriting worker.
	if err := manager.ReloadProxyURL(ctx, newProxy); err != nil {
		t.Fatalf("retry with the same value failed: %v", err)
	}
	if got := len(starter.accounts); got != 2 {
		t.Fatalf("retry did not restart the worker: starts = %d, want 2", got)
	}

	// Now that the reload succeeded, an identical value is a genuine no-op.
	if err := manager.ReloadProxyURL(ctx, newProxy); err != nil {
		t.Fatalf("post-success no-op reload: %v", err)
	}
	if got := len(starter.accounts); got != 2 {
		t.Fatalf("post-success identical value restarted the worker: starts = %d, want 2", got)
	}
}
