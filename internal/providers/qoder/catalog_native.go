package qoder

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Native model catalog (plan Task 11). The CLI fetches
// GET /api/v2/model/list?Encode=1 on the chat host, signed by the same WASM
// COSY mechanism as chat but via prepareRequest (request class "auth", empty
// body). The response is plaintext JSON keyed by scene (chat/developer/...);
// the Task 11 recording is in testdata/native/model_catalog_cn.json.
//
// Entry mapping reuses ModelInfos (the worker-path mapping) so both paths
// produce identical ModelInfo rows for the same upstream fields.

// catalogPath is the model catalog endpoint path with its query. The WASM
// signs it against the CHAT host (gateway/api1), not the openapi host; the
// openapi host answers with an ALB 503 for this path.
const catalogPath = "/algo/api/v2/model/list?Encode=1"

type catalogClient struct {
	HTTP      *http.Client
	Region    string
	Identity  inferIdentity
	Now       func() time.Time
	Entropy   io.Reader
	Endpoints map[string]nativeEndpoints
}

func (c catalogClient) endpointFor() (nativeEndpoints, bool) {
	if c.Endpoints != nil {
		if e, ok := c.Endpoints[c.Region]; ok {
			return e, true
		}
		return nativeEndpoints{}, false
	}
	return chatEndpoint(c.Region)
}

// chatEndpoint maps a region to its chat/gateway host for the signed catalog
// GET (Task 11 live: gateway.qoder.com.cn serves /algo/api/v2/model/list).
func chatEndpoint(region string) (nativeEndpoints, bool) {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "cn":
		return nativeEndpoints{base: "https://gateway.qoder.com.cn", origin: "https://qoder.com.cn"}, true
	case "global":
		return nativeEndpoints{base: "https://api1.qoder.sh", origin: "https://qoder.com"}, true
	default:
		return nativeEndpoints{}, false
	}
}

// signedCatalogRequest builds the GET request for the model catalog. The WASM
// prepareRequest signs with an empty body and the normalized path; the COSY
// authorization covers the "auth" request class exactly like chat.
func (c catalogClient) signedCatalogRequest(ctx context.Context, clock func() time.Time, entropy io.Reader) (*http.Request, error) {
	if clock == nil {
		clock = time.Now
	}
	endpoint, ok := c.endpointFor()
	if !ok {
		return nil, fmt.Errorf("qoder region %q has no openapi endpoint", c.Region)
	}
	if entropy == nil {
		entropy = rand.Reader
	}
	var random [16]byte
	if _, err := io.ReadFull(entropy, random[:]); err != nil {
		return nil, fmt.Errorf("read catalog entropy: %w", err)
	}
	requestID := formatCosyUUID(reverseMaskUUID(random))
	unixStr := fmt.Sprintf("%d", clock().Unix())
	// Empirically cracked (Task 11 live): the catalog signature preimage uses
	// the path WITHOUT /algo and WITHOUT the query string.
	signedPath := normalizeSignedPath("/algo/api/v2/model/list")

	payload := buildCosyPayload(c.Identity.Version, requestID, c.Identity.Info)
	payloadB64 := base64Std([]byte(payload))
	signature := cosySignature(payloadB64, c.Identity.Key, unixStr, "", signedPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.base+catalogPath, nil)
	if err != nil {
		return nil, err
	}
	// Header matrix from the WASM prepareRequest dump (Task 11, CN live):
	// model/list sends the chat matrix plus Cosy-Business-*/Scene/ClientIp,
	// Accept-Encoding identity, and a Content-Type despite the empty body.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer COSY."+payloadB64+"."+signature)
	req.Header.Set("Cosy-Business-Product", "cli")
	req.Header.Set("Cosy-Business-Type", "agent")
	req.Header.Set("Cosy-ClientIp", c.Identity.MachineID)
	req.Header.Set("Cosy-ClientType", "5")
	req.Header.Set("Cosy-Data-Policy", dataPolicyValue(c.Identity.DataPolicyAgreed))
	req.Header.Set("Cosy-Date", unixStr)
	req.Header.Set("Cosy-Key", c.Identity.Key)
	req.Header.Set("Cosy-Machineid", c.Identity.MachineID)
	req.Header.Set("Cosy-Machinetoken", c.Identity.MachineID)
	req.Header.Set("Cosy-Machinetype", "5")
	req.Header.Set("Cosy-Scene", "assistant")
	req.Header.Set("Cosy-User", c.Identity.UID)
	req.Header.Set("Cosy-Version", c.Identity.Version)
	req.Header.Set("Login-Version", "v2")
	return req, nil
}

// fetchCatalogRaw performs the signed GET and returns the raw chat-scene rows
// (map per entry) so the shared ModelInfos mapping can decode them.
func (c catalogClient) fetchCatalogRaw(ctx context.Context) ([]map[string]any, error) {
	req, err := c.signedCatalogRequest(ctx, c.Now, c.Entropy)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, TransportError{Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, HTTPStatusError{Op: "model_catalog", Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("qoder model catalog response: %w", err)
	}
	sceneRaw, ok := parsed["chat"]
	if !ok {
		return nil, fmt.Errorf("qoder model catalog carries no chat scene")
	}
	var entries []map[string]any
	if err := json.Unmarshal(sceneRaw, &entries); err != nil {
		return nil, fmt.Errorf("qoder model catalog chat scene: %w", err)
	}
	return entries, nil
}

// FetchModelsNative loads the model catalog for one account natively. It is
// the adapter.Models slot implementation on the native path.
func (c *Client) FetchModelsNative(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	account, cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	identity, err := buildChatIdentity(account, cred)
	if err != nil {
		return nil, err
	}
	httpClient, err := c.nativeHTTP(ctx, account)
	if err != nil {
		return nil, err
	}
	cc := catalogClient{
		HTTP:     httpClient,
		Region:   account.ProviderRegion,
		Identity: identity,
	}
	rawEntries, err := cc.fetchCatalogRaw(ctx)
	if err != nil {
		return nil, err
	}
	return ModelInfos(catalogSnapshotRows(rawEntries)), nil
}

// catalogSnapshotRows converts raw model/list rows into the worker snapshot
// row shape (id, mapped_key, display_name, ...) that ModelInfos consumes,
// mirroring worker/src/catalog.mjs createModelCatalogSnapshot: canonical
// id from display_name (falling back to key), one row per id.
func catalogSnapshotRows(entries []map[string]any) []map[string]any {
	rows := make([]map[string]any, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		key := stringField(entry, "key")
		if key == "" {
			continue
		}
		if enable, ok := entry["enable"].(bool); ok && !enable {
			continue
		}
		displayName := stringField(entry, "display_name")
		if displayName == "" {
			displayName = key
		}
		id := canonicalModelID(displayName)
		if seen[id] {
			continue
		}
		seen[id] = true
		row := map[string]any{
			"id":                 id,
			"display_name":       displayName,
			"mapped_key":         key,
			"route_display_name": displayName,
			"is_reasoning":       entry["is_reasoning"],
			"price_factor":       entry["price_factor"],
			"is_free":            entry["is_free"],
			"tags":               entry["tags"],
			"max_input_tokens":   entry["max_input_tokens"],
			"max_output_tokens":  entry["max_output_tokens"],
		}
		if src := stringField(entry, "source"); src != "" {
			row["source"] = src
		}
		rows = append(rows, row)
	}
	return rows
}
