package runtime_test

import (
	"fmt"
	"github.com/caigee-cmd/cli2api/internal/accounts"
	accountruntime "github.com/caigee-cmd/cli2api/internal/runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExecStarterSetProxyURLAppliesToNewWorkers(t *testing.T) {
	starter := &accountruntime.ExecStarter{Config: accountruntime.ManagerConfig{
		DaemonPath:   "/app/worker/daemon.mjs",
		QoderCLIPath: "/usr/lib/qodercli.js",
	}}

	starter.SetProxyURL("http://proxy.example:8080")
	env := starterEnvForTest(t, starter, accounts.Account{ID: "acc1", Provider: "qoder", ProviderRegion: "global", MaxInFlight: 4}, "/tmp/home", 32100)

	if got := envValue(env, "QODER_PROXY_URL"); got != "http://proxy.example:8080" {
		t.Fatalf("QODER_PROXY_URL = %q", got)
	}
	if got := envValue(env, "HTTPS_PROXY"); got != "http://proxy.example:8080" {
		t.Fatalf("HTTPS_PROXY = %q", got)
	}

	// A later update is visible to the next spawn.
	starter.SetProxyURL("  direct  ")
	env = starterEnvForTest(t, starter, accounts.Account{ID: "acc1", Provider: "qoder", ProviderRegion: "global", MaxInFlight: 4}, "/tmp/home", 32100)
	if got := envValue(env, "QODER_PROXY_URL"); got != "direct" {
		t.Fatalf("QODER_PROXY_URL after update = %q", got)
	}
	if got := envValue(env, "HTTPS_PROXY"); got != "" {
		t.Fatalf("direct must not inject HTTPS_PROXY, got %q", got)
	}
}

func TestStarterEnvAccountProxyOverridesGlobal(t *testing.T) {
	config := accountruntime.ManagerConfig{
		DaemonPath:   "/app/worker/daemon.mjs",
		QoderCLIPath: "/usr/lib/qodercli.js",
		ProxyURL:     "http://global.example:8080",
	}

	// accounts.Account override wins.
	env := starterEnvForTestConfig(t, config, accounts.Account{
		ID: "acc1", Provider: "qoder", ProviderRegion: "global", MaxInFlight: 4,
		ProxyURL: "http://account.example:9090",
	}, "/tmp/home", 32100)
	if got := envValue(env, "QODER_PROXY_URL"); got != "http://account.example:9090" {
		t.Fatalf("account override QODER_PROXY_URL = %q", got)
	}

	// accounts.Account "direct" beats the global HTTP proxy.
	env = starterEnvForTestConfig(t, config, accounts.Account{
		ID: "acc1", Provider: "qoder", ProviderRegion: "global", MaxInFlight: 4,
		ProxyURL: "direct",
	}, "/tmp/home", 32100)
	if got := envValue(env, "QODER_PROXY_URL"); got != "direct" {
		t.Fatalf("account direct QODER_PROXY_URL = %q", got)
	}
	if got := envValue(env, "HTTPS_PROXY"); got != "" {
		t.Fatalf("account direct must not inject HTTPS_PROXY, got %q", got)
	}

	// Empty account proxy inherits the global.
	env = starterEnvForTestConfig(t, config, accounts.Account{
		ID: "acc1", Provider: "qoder", ProviderRegion: "global", MaxInFlight: 4,
	}, "/tmp/home", 32100)
	if got := envValue(env, "QODER_PROXY_URL"); got != "http://global.example:8080" {
		t.Fatalf("inherited QODER_PROXY_URL = %q", got)
	}
}

func TestExecStarterConfigSnapshotConcurrentWithSetProxyURL(t *testing.T) {
	starter := &accountruntime.ExecStarter{Config: accountruntime.ManagerConfig{
		DaemonPath:   "/app/worker/daemon.mjs",
		QoderCLIPath: "/usr/lib/qodercli.js",
	}}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			starter.SetProxyURL("http://proxy.example:8080")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = starter.ConfigSnapshot()
		}
	}()
	wg.Wait()
}

func starterEnvForTest(t *testing.T, starter *accountruntime.ExecStarter, account accounts.Account, home string, port int) []string {
	t.Helper()
	return starterEnvForTestConfig(t, starter.ConfigSnapshot(), account, home, port)
}

func starterEnvForTestConfig(t *testing.T, config accountruntime.ManagerConfig, account accounts.Account, home string, port int) []string {
	t.Helper()
	env, err := accountruntime.StarterEnv(config, account, home, port)
	if err != nil {
		t.Fatalf("starterEnv: %v", err)
	}
	return env
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return strings.TrimPrefix(entry, key+"=")
		}
	}
	return ""
}

func TestExecStarterConcurrentKeyProxyAndSnapshot(t *testing.T) {
	for _, constructed := range []bool{false, true} {
		t.Run(fmt.Sprintf("constructor=%v", constructed), func(t *testing.T) {
			initial := accountruntime.ManagerConfig{DataDir: "runtime-dir", BasePort: 32100, NodeBinary: "node", DaemonPath: "daemon.mjs", QoderCLIPath: "qodercli.js", RestartDelay: time.Second}
			starter := &accountruntime.ExecStarter{Config: initial}
			if constructed {
				starter = accountruntime.NewExecStarter(initial)
			}
			var wg sync.WaitGroup
			wg.Add(3)
			go func() {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					starter.SetProxyURL(" http://proxy.example:8080 ")
				}
			}()
			go func() {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					starter.SetProxyAPIKey("rotated-key")
				}
			}()
			go func() {
				defer wg.Done()
				for i := 0; i < 500; i++ {
					cfg := starter.ConfigSnapshot()
					if cfg.DataDir != initial.DataDir || cfg.BasePort != initial.BasePort || cfg.RestartDelay != initial.RestartDelay {
						t.Error("snapshot lost runtime fields")
						return
					}
				}
			}()
			wg.Wait()
			cfg := starter.ConfigSnapshot()
			if cfg.ProxyURL != "http://proxy.example:8080" || cfg.ProxyAPIKey != "rotated-key" {
				t.Fatal("snapshot did not retain both updates")
			}
			env := starterEnvForTest(t, starter, accounts.Account{ID: "a", Provider: "qoder", ProviderRegion: "global"}, t.TempDir(), 32100)
			if envValue(env, "QODER_PROXY_URL") != cfg.ProxyURL || envValue(env, "PROXY_API_KEY") != cfg.ProxyAPIKey {
				t.Fatal("worker environment did not use final credentials/proxy")
			}
		})
	}
}
