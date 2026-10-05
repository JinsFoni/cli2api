package qoder

import "testing"

func TestBuildCosyPayloadFieldOrder(t *testing.T) {
	got := buildCosyPayload("1.1.32", "8d8c8b8a-8988-4786-8584-838281807f7e", "INFO")
	want := `{"version":"v1","requestId":"8d8c8b8a-8988-4786-8584-838281807f7e","info":"INFO","cosyVersion":"1.1.32","ideVersion":""}`
	if got != want {
		t.Fatalf("payload order mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestCosySignatureKnownVector(t *testing.T) {
	// 五段四换行、末尾无 LF 的 canonical MD5;向量由本测试定义,Task 8 用真机
	// 输出交叉验证(录制件里含同输入的 Authorization 头)。
	got := cosySignature("cGF5bG9hZA==", "the-key", "1781000123", "ENCODED", "/api/v2/service/pro/sse/agent_chat_generation")
	// md5("cGF5bG9hZA==\nthe-key\n1781000123\nENCODED\n/api/v2/..") 固定值
	want := "cbb47dbb94bafe7992fa9d83e79f82f4"
	if got != want {
		t.Fatalf("signature mismatch: got %s want %s", got, want)
	}
}

func TestNormalizeSignedPath(t *testing.T) {
	if got := normalizeSignedPath("/algo/api/v2/service/pro/sse/agent_chat_generation"); got != "/api/v2/service/pro/sse/agent_chat_generation" {
		t.Fatalf("got %s", got)
	}
}
