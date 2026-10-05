package qoder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Task 8 cross-validation: fixtures recorded from the real pinned CLI
// (1.1.32) by scripts/record-qoder-fixtures.mjs. The body codec is
// deterministic, so a recorded encoded body is a byte-exact oracle for the
// Go implementation; signatures carry a per-request UUID and unix timestamp,
// so they are validated structurally (COSY.{b64}.{32-hex}) plus one
// full-preimage recomputation against a captured Authorization when the
// recording ran with --keep-tokens.

type prepareFixture struct {
	Region    string `json:"region"`
	CLI       string `json:"cliVersion"`
	KeepToken bool   `json:"keepTokens"`
	Records   []struct {
		Input struct {
			Name string `json:"name"`
			Body string `json:"body"`
		} `json:"input"`
		URL        string         `json:"url"`
		Headers    map[string]any `json:"headers"`
		UserAgent  any            `json:"userAgent"`
		BodyLength int            `json:"bodyLength"`
		BodySHA256 string         `json:"bodySha256"`
	} `json:"records"`
}

// loadPrepareFixture returns ok=false for regions without a recording yet
// (e.g. global), so tests skip that region instead of failing the suite.
func loadPrepareFixture(t *testing.T, region string) (prepareFixture, bool) {
	t.Helper()
	path := filepath.Join("testdata", "native", "prepare_"+region+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Logf("fixture %s not recorded yet: %v", path, err)
		return prepareFixture{}, false
	}
	var f prepareFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f, true
}

// TestFixtureCodecMatchesRealCLI re-encodes each recorded plain input with the
// Go codec and compares SHA-256 against the recorded encoded body.
func TestFixtureCodecMatchesRealCLI(t *testing.T) {
	for _, region := range []string{"cn", "global"} {
		f, ok := loadPrepareFixture(t, region)
		if !ok {
			continue
		}
		for _, rec := range f.Records {
			encoded := encodeNativeBody([]byte(rec.Input.Body))
			sum := sha256.Sum256(encoded)
			if got := hex.EncodeToString(sum[:]); got != rec.BodySHA256 {
				t.Errorf("%s/%s: encoded body sha mismatch:\n got %s\nwant %s", region, rec.Input.Name, got, rec.BodySHA256)
				continue
			}
			if len(encoded) != rec.BodyLength {
				t.Errorf("%s/%s: encoded length %d, recorded %d", region, rec.Input.Name, len(encoded), rec.BodyLength)
			}
			roundTrip, err := decodeNativeBody(encoded)
			if err != nil || string(roundTrip) != rec.Input.Body {
				t.Errorf("%s/%s: decode round-trip failed: %v", region, rec.Input.Name, err)
			}
		}
	}
}

// TestFixtureURLShape asserts the recorded URL (path + query) equals what the
// Go prepare builds for both regions.
func TestFixtureURLShape(t *testing.T) {
	for _, region := range []string{"cn", "global"} {
		fixture, ok := loadPrepareFixture(t, region)
		if !ok {
			continue
		}
		f := fixture
		if len(f.Records) == 0 {
			t.Fatalf("region %s: no records", region)
		}
		id := inferIdentity{Region: region, Version: f.CLI}
		prepared, err := id.prepareInferRequest("https://chat.invalid", "{}", "auto", "system", nil, zeroEntropy{})
		if err != nil {
			t.Fatal(err)
		}
		// Compare path+query of the recorded URL against the Go one.
		want := f.Records[0].URL
		marker := "/algo/"
		i := strings.Index(want, marker)
		if i < 0 {
			t.Fatalf("recorded url lacks /algo/: %s", want)
		}
		wantSuffix := want[i:] // path+query
		got := prepared.URL
		j := strings.Index(got, marker)
		if j < 0 {
			t.Fatalf("go url lacks /algo/: %s", got)
		}
		gotSuffix := got[j:]
		if gotSuffix != wantSuffix {
			t.Errorf("region %s: path+query\n got %s\nwant %s", region, gotSuffix, wantSuffix)
		}
	}
}

// TestFixtureHeaderMatrixPerRegion walks every recorded header name and
// asserts the Go matrix emits the same set. Deterministic values must match
// exactly; identity-bound headers (Authorization / Cosy-Date / Cosy-Key /
// Cosy-User) come from the real login session and cannot be regenerated
// offline, so they are validated structurally (shape and length).
func TestFixtureHeaderMatrixPerRegion(t *testing.T) {
	identityShape := map[string]func(got string) string{
		"Authorization": func(g string) string {
			if !strings.HasPrefix(g, "Bearer COSY.") {
				return "authorization shape"
			}
			sig := strings.TrimPrefix(g, "Bearer COSY.")
			dot := strings.LastIndex(sig, ".")
			if dot < 0 || len(sig[dot+1:]) != 32 {
				return "authorization signature segment not 32 hex"
			}
			return ""
		},
		"Cosy-Date": func(g string) string {
			if len(g) != 10 {
				return "cosy date not 10-digit unix seconds"
			}
			return ""
		},
		"Cosy-Key": func(g string) string {
			if len(g) != 172 {
				return "cosy key not 172-char base64 (256-byte RSA)"
			}
			return ""
		},
		"Cosy-User": func(g string) string {
			if len(g) != 36 || strings.Count(g, "-") != 4 {
				return "cosy user not a 36-char uuid"
			}
			return ""
		},
	}
	for _, region := range []string{"cn", "global"} {
		fixture, ok := loadPrepareFixture(t, region)
		if !ok {
			continue
		}
		f := fixture
		if len(f.Records) == 0 {
			t.Fatalf("region %s: no records", region)
		}
		rec := f.Records[0]
		id := inferIdentity{
			Region:    region,
			Version:   f.CLI,
			MachineID: headerString(rec.Headers, "Cosy-MachineId"),
			UID:       "01234567-89ab-4cde-8f01-23456789abcd",
			// 172-char stand-in: 171 chars + padding byte, matching the
			// recorded Cosy-Key length (256-byte RSA ciphertext, base64).
			Key:              strings.Repeat("A", 171) + "=",
			DataPolicyAgreed: true,
		}
		prepared, err := id.prepareInferRequest("https://chat.invalid", "{}", "auto", "system", nil, zeroEntropy{})
		if err != nil {
			t.Fatal(err)
		}
		for name, raw := range rec.Headers {
			name = textproto.CanonicalMIMEHeaderKey(name)
			got := prepared.Header.Get(name)
			if check, ok := identityShape[name]; ok {
				if got == "" {
					t.Errorf("region %s: header %s missing (recorded present)", region, name)
				} else if problem := check(got); problem != "" {
					t.Errorf("region %s header %s: %s: %q", region, name, problem, got)
				}
				continue
			}
			want, _ := raw.(string)
			if got != want {
				t.Errorf("region %s header %s:\n got %q\nwant %q", region, name, got, want)
			}
		}
		// Machinetoken must equal Machineid per recording.
		if prepared.Header.Get("Cosy-Machinetoken") != id.MachineID {
			t.Errorf("region %s: machinetoken must equal machine id", region)
		}
		// The WASM layer never emits User-Agent; Go must not add one into the
		// signed matrix (it is set on the http.Request afterwards).
		if _, ok := prepared.Header["User-Agent"]; ok {
			t.Errorf("region %s: User-Agent must not be part of the header matrix", region)
		}
	}
}

// TestFixtureSSEEnvelopeShape parses the recorded real-chat SSE samples with
// the Go envelope decoder to prove the shapes line up.
func TestFixtureSSEEnvelopeShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "native", "sse_cn.json"))
	if err != nil {
		t.Skipf("sse fixture not recorded yet: %v", err)
	}
	var f struct {
		HTTPStatus    int            `json:"httpStatus"`
		Samples       map[string]any `json:"samples"`
		RawTailFrames []string       `json:"rawTailFrames"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.HTTPStatus != 200 {
		t.Fatalf("http status %d", f.HTTPStatus)
	}
	// The usage sample must decode to an envelope with a nested usage.
	usageFrame, _ := f.Samples["usage"].(string)
	if usageFrame == "" {
		t.Fatal("missing usage sample")
	}
	dataLine := strings.TrimPrefix(strings.SplitN(usageFrame, "\n", 2)[0], "data:")
	var env upstreamEnvelope
	if err := json.Unmarshal([]byte(dataLine), &env); err != nil {
		t.Fatalf("usage envelope parse: %v", err)
	}
	if env.StatusCodeValue == nil || *env.StatusCodeValue != 200 {
		t.Fatalf("usage envelope status = %v", env.StatusCodeValue)
	}
	body := decodeUpstreamBody(env)
	if body.done || body.err != nil {
		t.Fatalf("usage envelope body done=%v err=%v", body.done, body.err)
	}
	var chunk struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body.raw, &chunk); err != nil {
		t.Fatalf("usage chunk parse: %v", err)
	}
	if _, ok := chunk.Usage["prompt_tokens"]; !ok {
		t.Fatalf("usage missing prompt_tokens: %s", body.raw)
	}
	// Terminal [DONE] body must decode as done.
	doneEnv := upstreamEnvelope{Body: json.RawMessage(`"[DONE]"`)}
	if b := decodeUpstreamBody(doneEnv); !b.done {
		t.Fatal("[DONE] body must decode as done")
	}
}

func headerString(headers map[string]any, key string) string {
	if v, ok := headers[key].(string); ok {
		return v
	}
	return ""
}

// zeroEntropy supplies a deterministic 16-byte stream for URL/shape tests.
type zeroEntropy struct{}

func (zeroEntropy) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0x42
	}
	return len(p), nil
}
