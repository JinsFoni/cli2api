package agnes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelsParsesCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathModels {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test_key_1234567890" {
			t.Fatalf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"agnes-2.5-flash","object":"model"},
			{"id":"agnes-2.5-pro","object":"model"},
			{"id":"agnes-image-2.5-flash","object":"model"},
			{"id":"agnes-video-2.5","object":"model"}
		]}`))
	}))
	defer srv.Close()
	client, _ := newTestClient(t, srv)

	models, err := client.Models(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 text models, got %d: %+v", len(models), models)
	}
	if models[0].NativeModel != "agnes-2.5-flash" || models[0].PublicModel != "agnes-2.5-flash" {
		t.Fatalf("unexpected model: %+v", models[0])
	}
	if !models[0].Capabilities.Tools {
		t.Fatal("text models should advertise tool support")
	}
	if models[0].Capabilities.Reasoning {
		t.Fatal("reasoning must not be invented for undeclared models")
	}
}

func TestModelsCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"agnes-2.5-flash"}]}`))
	}))
	defer srv.Close()
	client, _ := newTestClient(t, srv)

	if _, err := client.Models(context.Background(), "acc-1"); err != nil {
		t.Fatalf("first Models: %v", err)
	}
	srv.Close() // A second call must hit the TTL cache, not the network.
	if _, err := client.Models(context.Background(), "acc-1"); err != nil {
		t.Fatalf("cached Models: %v", err)
	}
}

func TestProbeValidatesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathModels {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Token not provided"}}`))
	}))
	defer srv.Close()
	client, store := newTestClient(t, srv)

	health, err := client.Probe(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Probe returned an error (should report via health): %v", err)
	}
	if health.Ready {
		t.Fatal("Probe reported ready for an invalid key")
	}
	if !strings.Contains(health.LastError, "Token not provided") {
		t.Fatalf("LastError = %q", health.LastError)
	}
	if _, observed := store.observed["acc-1"]; observed {
		t.Fatal("failed probe must not mark the account ready")
	}

	// Now a healthy server flips the account ready.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"agnes-2.5-flash"}]}`))
	}))
	defer ok.Close()
	client2, store2 := newTestClient(t, ok)
	health, err = client2.Probe(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !health.Ready || !health.Hot {
		t.Fatalf("health = %+v", health)
	}
	if store2.observed["acc-1"] != "ready" {
		t.Fatalf("observed status = %q", store2.observed["acc-1"])
	}
}

func TestQuotaIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client, _ := newTestClient(t, srv)
	quota, err := client.Quota(context.Background(), "acc-1")
	if err != nil || quota != nil {
		t.Fatalf("Quota = (%v, %v), want (nil, nil)", quota, err)
	}
}

func TestParseCatalogJSONFiltersNonText(t *testing.T) {
	models, err := ParseCatalogJSON([]byte(`{"data":[{"id":"agnes-image-2.5-flash"},{"id":"agnes-video-2.5"},{"id":"x"}]}`))
	if err != nil {
		t.Fatalf("ParseCatalogJSON: %v", err)
	}
	if len(models) != 1 || models[0].NativeModel != "x" {
		t.Fatalf("filter failed: %+v", models)
	}
	if _, err := ParseCatalogJSON([]byte(`{"data":[]}`)); err == nil {
		t.Fatal("empty catalog must error")
	}
}
