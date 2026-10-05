package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Task 11: native model catalog. The fixture is a real CN catalog capture
// (WASM-signed GET via the Task 8/10 recorder stack); the fake-upstream tests
// verify the Go signed request and the shared ModelInfos mapping.

// TestCatalogNativeFixtureMapping runs the recorded real-catalog rows through
// ModelInfos and asserts the routing ids and capability flags the executor
// and console rely on.
type ModelInfoRow = providers.ModelInfo

func TestCatalogNativeFixtureMapping(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "native", "model_catalog_cn.json"))
	if err != nil {
		t.Skipf("catalog fixture not recorded yet: %v", err)
	}
	var scenes map[string][]map[string]any
	if err := json.Unmarshal(raw, &scenes); err != nil {
		t.Fatalf("parse catalog fixture: %v", err)
	}
	infos := ModelInfos(catalogSnapshotRows(scenes["chat"]))
	if len(infos) == 0 {
		t.Fatal("no models mapped from fixture")
	}
	byPublic := map[string]ModelInfoRow{}
	for _, info := range infos {
		byPublic[strings.ToLower(info.PublicModel)] = info
	}
	auto, ok := byPublic["auto"]
	if !ok {
		t.Fatalf("auto model missing from %d infos", len(infos))
	}
	if auto.NativeModel != "auto" || auto.DisplayName != "Auto" {
		t.Fatalf("auto = %+v", auto)
	}
	if !auto.Capabilities.Reasoning || !auto.Capabilities.Images {
		t.Fatalf("auto capabilities = %+v", auto.Capabilities)
	}
	// glM-5.3 / "GLM-5.3" must route on a canonical lowercase id.
	glm, ok := byPublic["glm-5.3"]
	if !ok {
		t.Fatalf("glm-5.3 missing; have %d infos", len(infos))
	}
	if glm.NativeModel != "gmodel" {
		t.Fatalf("glm native key = %q", glm.NativeModel)
	}
	// price_factor 0.5 + is_free true must render the worker-parity credits.
	if glm.Free {
		t.Fatalf("glm-5.3 must not be free (positive price factor)")
	}
}

// TestCatalogNativeSignedRequest asserts the Go signed GET reaches the fake
// upstream with COSY headers and returns parsed models.
func TestCatalogNativeSignedRequest(t *testing.T) {
	var sawAuth, sawDate, sawKey, sawPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawDate = r.Header.Get("Cosy-Date")
		sawKey = r.Header.Get("Cosy-Key")
		sawPath = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"chat":[{"key":"gmodel","display_name":"GLM-5.3","source":"system","enable":true,"is_reasoning":true,"is_vl":true,"price_factor":0.5,"max_input_tokens":180000}]}`))
	}))
	defer upstream.Close()

	cc := catalogClient{
		HTTP:     upstream.Client(),
		Region:   "cn",
		Identity: inferTestIdentity("cn"),
		Endpoints: map[string]nativeEndpoints{
			"cn":     {base: upstream.URL, origin: upstream.URL},
			"global": {base: upstream.URL, origin: upstream.URL},
		},
	}
	entries, err := cc.fetchCatalogRaw(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0]["key"] != "gmodel" {
		t.Fatalf("entries = %+v", entries)
	}
	if !strings.HasPrefix(sawAuth, "Bearer COSY.") {
		t.Fatalf("auth = %q", sawAuth)
	}
	if sawDate == "" || sawKey == "" {
		t.Fatalf("cosy date/key missing: %q %q", sawDate, sawKey)
	}
	if sawPath != "/algo/api/v2/model/list?Encode=1" {
		t.Fatalf("request URI = %q", sawPath)
	}
	infos := ModelInfos(catalogSnapshotRows(entries))
	if len(infos) != 1 || infos[0].NativeModel != "gmodel" {
		t.Fatalf("infos = %+v", infos)
	}
}

// TestCatalogNativeFixtureLiveRequest signs the fixture-fresh identity against
// the recorded catalog endpoint shape and verifies request construction only.
func TestCatalogNativeRequestShape(t *testing.T) {
	cc := catalogClient{Region: "cn", Identity: inferTestIdentity("cn")}
	req, err := cc.signedCatalogRequest(context.Background(), inferTestClock, zeroEntropy{})
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Path != "/algo/api/v2/model/list" || !strings.Contains(req.URL.RawQuery, "Encode=1") {
		t.Fatalf("url = %s", req.URL.String())
	}
	if req.Header.Get("Cosy-Machinetoken") != inferTestIdentity("cn").MachineID {
		t.Fatalf("machinetoken = %q", req.Header.Get("Cosy-Machinetoken"))
	}
	if _, ok := req.Header["User-Agent"]; ok {
		t.Fatal("catalog request must not carry a User-Agent in the signed matrix")
	}
}
