package qoder

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func inferTestClock() time.Time { return time.Unix(1781000123, 0) }

func inferTestIdentity(region string) inferIdentity {
	return inferIdentity{
		MachineID:        "0f1e2d3c-4b5a-4968-8776-655443332211",
		UID:              "user-1",
		Info:             "INFO-B64",
		Key:              "COSYKEY-B64",
		Version:          "1.1.32",
		OrganizationID:   "org-1",
		OrganizationTags: []string{"team-a", "team-b"},
		DataPolicyAgreed: true,
		Region:           region,
	}
}

func mustPrepare(t *testing.T, id inferIdentity, entropy []byte, modelKey, modelSource string) preparedInfer {
	t.Helper()
	prepared, err := id.prepareInferRequest("https://chat.example", `{"x":1}`, modelKey, modelSource,
		inferTestClock, bytes.NewReader(entropy))
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestPrepareInferFullOrgHeaderMatrix(t *testing.T) {
	prepared := mustPrepare(t, inferTestIdentity("global"), bytes.Repeat([]byte{0xAA}, 16), "gpt-x", "catalog")
	if prepared.URL != "https://chat.example/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1" {
		t.Fatalf("url: %s", prepared.URL)
	}
	if got := len(prepared.Header); got != 22 {
		t.Fatalf("header count %d, want 22: %v", got, prepared.Header)
	}
	want := map[string]string{
		"Accept":                 "text/event-stream",
		"Cache-Control":          "no-cache",
		"Connection":             "keep-alive",
		"Content-Type":           "application/json",
		"Cosy-Business-Product":  "cli",
		"Cosy-Business-Type":     "agent",
		"Cosy-Clienttype":        "5",
		"Cosy-Data-Policy":       "agree",
		"Cosy-Date":              "1781000123",
		"Cosy-Key":               "COSYKEY-B64",
		"Cosy-Machineid":         "0f1e2d3c-4b5a-4968-8776-655443332211",
		"Cosy-Machinetoken":      "0f1e2d3c-4b5a-4968-8776-655443332211",
		"Cosy-Machinetype":       "5",
		"Cosy-Organization-Id":   "org-1",
		"Cosy-Organization-Tags": "team-a,team-b",
		"Cosy-Scene":             "assistant",
		"Cosy-User":              "user-1",
		"Cosy-Version":           "1.1.32",
		"Login-Version":          "v2",
		"X-Model-Key":            "gpt-x",
		"X-Model-Source":         "catalog",
	}
	for k, v := range want {
		if got := prepared.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	auth := prepared.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer COSY.") || !strings.Contains(auth, ".") {
		t.Fatalf("authorization shape: %s", auth)
	}
}

func TestPrepareInferNoOrg(t *testing.T) {
	id := inferTestIdentity("global")
	id.OrganizationID = ""
	id.OrganizationTags = nil
	prepared := mustPrepare(t, id, bytes.Repeat([]byte{0xBB}, 16), "gpt-x", "catalog")
	if prepared.Header.Get("Cosy-Organization-Id") != "" || prepared.Header.Get("Cosy-Organization-Tags") != "" {
		t.Fatal("org headers must be absent")
	}
	if got := len(prepared.Header); got != 20 {
		t.Fatalf("header count %d, want 20", got)
	}
}

func TestPrepareInferOrgNoTags(t *testing.T) {
	id := inferTestIdentity("global")
	id.OrganizationTags = []string{}
	prepared := mustPrepare(t, id, bytes.Repeat([]byte{0xCC}, 16), "gpt-x", "catalog")
	if prepared.Header.Get("Cosy-Organization-Id") != "org-1" {
		t.Fatal("org id must stay")
	}
	if prepared.Header.Get("Cosy-Organization-Tags") != "" {
		t.Fatal("tags header must be absent for empty slice")
	}
	if got := len(prepared.Header); got != 21 {
		t.Fatalf("header count %d, want 21", got)
	}
}

func TestPrepareInferTagsNoOrg(t *testing.T) {
	id := inferTestIdentity("global")
	id.OrganizationID = ""
	prepared := mustPrepare(t, id, bytes.Repeat([]byte{0xDD}, 16), "gpt-x", "catalog")
	if prepared.Header.Get("Cosy-Organization-Tags") != "team-a,team-b" {
		t.Fatal("tags header must stay")
	}
	if prepared.Header.Get("Cosy-Organization-Id") != "" {
		t.Fatal("org id must be absent")
	}
	if got := len(prepared.Header); got != 21 {
		t.Fatalf("header count %d, want 21", got)
	}
}

func TestPrepareInferNoModelKey(t *testing.T) {
	id := inferTestIdentity("global")
	id.OrganizationID = ""
	id.OrganizationTags = nil
	prepared := mustPrepare(t, id, bytes.Repeat([]byte{0xEE}, 16), "", "")
	if prepared.Header.Get("X-Model-Key") != "" || prepared.Header.Get("X-Model-Source") != "" {
		t.Fatal("model headers must be absent")
	}
	if got := len(prepared.Header); got != 18 {
		t.Fatalf("header count %d, want 18", got)
	}
}

func TestPrepareInferEmptyModelSource(t *testing.T) {
	id := inferTestIdentity("global")
	id.OrganizationID = ""
	id.OrganizationTags = nil
	prepared := mustPrepare(t, id, bytes.Repeat([]byte{0xEF}, 16), "k", "")
	if _, ok := prepared.Header["X-Model-Source"]; !ok {
		t.Fatal("X-Model-Source must be present even when empty")
	}
	if prepared.Header.Get("X-Model-Source") != "" {
		t.Fatal("X-Model-Source must be empty")
	}
}

func TestPrepareInferSignatureClosure(t *testing.T) {
	prepared := mustPrepare(t, inferTestIdentity("global"), bytes.Repeat([]byte{0x11}, 16), "gpt-x", "catalog")
	auth := prepared.Header.Get("Authorization")
	rest := strings.TrimPrefix(auth, "Bearer COSY.")
	parts := strings.Split(rest, ".")
	if len(parts) != 2 {
		t.Fatalf("authorization payload/sig split: %s", auth)
	}
	payloadRaw, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("payload base64: %v", err)
	}
	var payload struct {
		Version     string `json:"version"`
		RequestID   string `json:"requestId"`
		Info        string `json:"info"`
		CosyVersion string `json:"cosyVersion"`
		IdeVersion  string `json:"ideVersion"`
	}
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if payload.Version != "v1" || payload.Info != "INFO-B64" || payload.CosyVersion != "1.1.32" || payload.IdeVersion != "" {
		t.Fatalf("payload fields: %+v", payload)
	}
	sig := cosySignature(parts[0], prepared.Header.Get("Cosy-Key"), prepared.Header.Get("Cosy-Date"),
		prepared.Body, normalizeSignedPath(inferPath))
	if sig != parts[1] {
		t.Fatalf("signature mismatch: recomputed %s, header %s", sig, parts[1])
	}
}

func TestPrepareInferZeroEntropyUUID(t *testing.T) {
	prepared := mustPrepare(t, inferTestIdentity("global"), make([]byte, 16), "", "")
	auth := prepared.Header.Get("Authorization")
	rest := strings.TrimPrefix(auth, "Bearer COSY.")
	parts := strings.Split(rest, ".")
	payloadRaw, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RequestID != "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("requestId: %s", payload.RequestID)
	}
}

func TestPrepareInferRequestIDVaries(t *testing.T) {
	a := mustPrepare(t, inferTestIdentity("global"), bytes.Repeat([]byte{0x01}, 16), "", "")
	b := mustPrepare(t, inferTestIdentity("global"), bytes.Repeat([]byte{0x02}, 16), "", "")
	if a.Header.Get("Authorization") == b.Header.Get("Authorization") {
		t.Fatal("different entropy must yield different requestId")
	}
}

func TestPrepareInferGlobalBodyEncoded(t *testing.T) {
	prepared := mustPrepare(t, inferTestIdentity("global"), bytes.Repeat([]byte{0x21}, 16), "", "")
	decoded, err := decodeNativeBody([]byte(prepared.Body))
	if err != nil {
		t.Fatalf("body must decode: %v", err)
	}
	if string(decoded) != `{"x":1}` {
		t.Fatalf("decoded body: %s", decoded)
	}
}

func TestPrepareInferCNBranch(t *testing.T) {
	id := inferTestIdentity("cn")
	prepared := mustPrepare(t, id, bytes.Repeat([]byte{0x31}, 16), "", "")
	if prepared.URL != "https://chat.example/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common" {
		t.Fatalf("cn url: %s", prepared.URL)
	}
	if prepared.Body != `{"x":1}` {
		t.Fatalf("cn body must be plaintext JSON: %s", prepared.Body)
	}
	if prepared.Header.Get("Appcode") != "cosy" {
		t.Fatal("Appcode missing")
	}
	if ip := prepared.Header.Get("Cosy-Clientip"); len(ip) != 36 || strings.Count(ip, "-") != 4 {
		t.Fatalf("clientip not UUID-form: %q", ip)
	}
	if prepared.Header.Get("Cosy-Machineos") == "" {
		t.Fatal("Cosy-Machineos missing")
	}
	if _, ok := prepared.Header["Cosy-Machinetoken"]; !ok {
		t.Fatal("Cosy-Machinetoken must be present (empty) for cn")
	}
	if prepared.Header.Get("Cosy-Machinetoken") != "" {
		t.Fatal("cn machinetoken must be empty")
	}
	if prepared.Header.Get("Cosy-Data-Policy") != "" {
		t.Fatal("cn must omit Cosy-Data-Policy (until Task 8 recording)")
	}
	if prepared.Header.Get("Cosy-Organization-Id") != "" || prepared.Header.Get("Cosy-Organization-Tags") != "" {
		t.Fatal("cn must omit org headers (until Task 8 recording)")
	}
}
