package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Task 10: native PAT login. The fake openapi serves jobToken/exchange and
// userinfo; the resulting blob must decode with the standard decoder and
// carry the runtime field pair so chat can sign immediately.

func fakeOpenapi(t *testing.T, handler http.HandlerFunc) loginNativeClient {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	return loginNativeClient{
		Region:  "global",
		Entropy: zeroEntropy{},
		Endpoints: map[string]nativeEndpoints{
			"global": {base: upstream.URL, origin: upstream.URL},
			"cn":     {base: upstream.URL, origin: upstream.URL},
		},
	}
}

func TestLoginPATExchangeAndBlobRoundTrip(t *testing.T) {
	var exchangeCalls, userinfoCalls int
	client := fakeOpenapi(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/exchange":
			exchangeCalls++
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("exchange content-type = %q", r.Header.Get("Content-Type"))
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["personal_token"] != "pt-test-token" {
				t.Errorf("exchange personal_token = %q", body["personal_token"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":                    "jt-new",
				"refresh_token":            "jrt-new",
				"expires_in":               3600,
				"refresh_token_expires_in": 7200,
			})
		case "/api/v1/userinfo":
			userinfoCalls++
			if r.Header.Get("Authorization") != "Bearer jt-new" {
				t.Errorf("userinfo auth = %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"uid": "u-native", "name": "Native User"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	blob, machineID, err := client.LoginPATNative(context.Background(), "pt-test-token")
	if err != nil {
		t.Fatal(err)
	}
	if exchangeCalls != 1 || userinfoCalls != 1 {
		t.Fatalf("calls exchange=%d userinfo=%d", exchangeCalls, userinfoCalls)
	}
	if len(machineID) != 36 || strings.Count(machineID, "-") != 4 {
		t.Fatalf("machineID = %q", machineID)
	}
	if blob.UID != "u-native" || blob.Name != "Native User" {
		t.Fatalf("blob identity = %+v", blob)
	}
	if blob.AccessToken != "jt-new" || blob.RefreshToken != "jrt-new" {
		t.Fatalf("blob tokens = %+v", blob)
	}
	if blob.ExpireTime == 0 {
		t.Fatal("blob expire_time missing")
	}
	if blob.SecurityOauthToken != "jt-new" {
		t.Fatalf("security token = %q", blob.SecurityOauthToken)
	}
	// The persisted JSON must carry the runtime field pair like the CLI's
	// blob (Task 8 dump).
	var stored map[string]any
	if err := json.Unmarshal(blob.Raw, &stored); err != nil {
		t.Fatal(err)
	}
	info, _ := stored["encrypt_user_info"].(string)
	key, _ := stored["key"].(string)
	if info == "" || key == "" {
		t.Fatalf("blob missing runtime fields: info=%q key=%q", info, key)
	}
	if len(key) == 0 {
		t.Fatal("key empty")
	}
	if stored["login_method"] != "token" {
		t.Fatalf("login_method = %v", stored["login_method"])
	}
	if stored["personal_access_token"] != "pt-test-token" {
		t.Fatalf("personal_access_token = %v", stored["personal_access_token"])
	}
}

func TestLoginPATRejectedSurfacesError(t *testing.T) {
	client := fakeOpenapi(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"TOKEN_EXPIRE"}`, http.StatusForbidden)
	})
	_, _, err := client.LoginPATNative(context.Background(), "pt-bad")
	if err == nil {
		t.Fatal("bad PAT must fail")
	}
	if !strings.Contains(err.Error(), "PAT rejected") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoginPATUnknownRegionFails(t *testing.T) {
	client := loginNativeClient{Region: "eu"}
	if _, _, err := client.LoginPATNative(context.Background(), "pt-x"); err == nil {
		t.Fatal("unknown region must fail")
	}
}

// TestLoginBlobDecodesBack proves the produced JSON survives the standard
// decode path, so a blob written by native login behaves like one the CLI
// wrote (imported accounts, refresh write-back).
func TestLoginBlobDecodesBack(t *testing.T) {
	client := fakeOpenapi(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/exchange":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "jt-x", "refresh_token": "jrt-x", "expires_in": 60})
		case "/api/v1/userinfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"uid": "uid-1", "name": "n"})
		default:
			http.NotFound(w, r)
		}
	})
	client.Region = "cn"
	client.Now = func() time.Time { return time.Unix(1781000000, 0) }

	blob, machineID, err := client.LoginPATNative(context.Background(), "pt-round")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeUserBlob(blob.Raw, machineID)
	if err != nil {
		t.Fatalf("decode produced blob: %v", err)
	}
	// decodeUserBlob leaves MachineID empty for plaintext JSON; identity
	// fields and the bearer token are what the chat path consumes.
	if decoded.UID != "uid-1" {
		t.Fatalf("decoded = %+v", decoded)
	}
	if decoded.bearerToken() != "jt-x" {
		t.Fatalf("bearer token = %q", decoded.bearerToken())
	}
	// Refresh routing must see the job token.
	if route, ok := refreshRoute(decoded.RefreshToken); !ok || route != "/api/v1/jobToken/refresh" {
		t.Fatalf("refresh route = %q ok=%v", route, ok)
	}
}
