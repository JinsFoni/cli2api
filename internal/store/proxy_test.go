package store

import (
	"context"
	"github.com/caigee-cmd/cli2api/internal/accounts"
	"path/filepath"
	"testing"
)

func TestValidateAccountProxy(t *testing.T) {
	// Native path: every provider is in-process and the shared Go transport
	// handles http(s), socks5, and socks5h, so no scheme is provider-gated.
	ok := []struct {
		provider string
		region   string
		raw      string
	}{
		{provider: "qoder", region: "global", raw: ""},
		{provider: "qoder", region: "global", raw: "direct"},
		{provider: "qoder", region: "cn", raw: "http://proxy.example:8080"},
		{provider: "qoder", region: "global", raw: "https://proxy.example:8443"},
		{provider: "qoder", region: "global", raw: "socks5://proxy.example:1080"},
		{provider: "qoder", region: "cn", raw: "socks5h://proxy.example:1080"},
		{provider: "workbuddy", raw: "socks5://proxy.example:1080"},
		{provider: "trae", raw: "socks5h://proxy.example:1080"},
	}
	for _, test := range ok {
		if err := accounts.ValidateAccountProxy(test.provider, test.region, test.raw); err != nil {
			t.Fatalf("ValidateAccountProxy(%q,%q,%q) = %v, want nil", test.provider, test.region, test.raw, err)
		}
	}
}

func TestStoreCreateAcceptsQoderSOCKS(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "QoderSocks", Enabled: true, ProxyURL: "socks5://proxy.example:1080"}); err != nil {
		t.Fatalf("Qoder account with SOCKS proxy rejected: %v", err)
	}
	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "QoderHTTP", Enabled: true, ProxyURL: "http://proxy.example:8080"}); err != nil {
		t.Fatalf("Qoder account with HTTP proxy rejected: %v", err)
	}
	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "WbSocks", Provider: "workbuddy", ProxyURL: "socks5://proxy.example:1080"}); err != nil {
		t.Fatalf("WorkBuddy account with SOCKS proxy rejected: %v", err)
	}
	if _, err := store.Create(ctx, accounts.CreateAccount{Name: "TraeSocks", Provider: "trae", ProxyURL: "socks5://proxy.example:1080"}); err != nil {
		t.Fatalf("Trae account with SOCKS proxy rejected: %v", err)
	}
}

func TestStoreUpdateProxyRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	account, err := store.Create(ctx, accounts.CreateAccount{Name: "QoderUpdate", Enabled: true, ProxyURL: "http://proxy.example:8080"})
	if err != nil {
		t.Fatal(err)
	}

	socks := "socks5://proxy.example:1080"
	if err := store.Update(ctx, account.ID, accounts.UpdateAccount{ProxyURL: &socks}); err != nil {
		t.Fatalf("proxy update to SOCKS rejected: %v", err)
	}

	reloaded, err := store.Get(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ProxyURL != socks {
		t.Fatalf("stored proxy = %q, want %q", reloaded.ProxyURL, socks)
	}
}
