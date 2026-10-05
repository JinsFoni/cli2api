package qoder

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// inferPath 是 chat 推理端点;签名 path 由 normalizeSignedPath 去掉 /algo。
const inferPath = "/algo/api/v2/service/pro/sse/agent_chat_generation"

// cnEncodeBody 控制 CN 分支 body 是否走编码产物。Task 8 真机录制
// (testdata/native/prepare_cn.json)确认 CN URL 携带 Encode=1 且 body 为
// 编码串,与 global 完全一致。
const cnEncodeBody = true

// cosyUserAgent 由调用方(chat_native 的 HTTP 层)设置。真机录制确认 WASM
// prepareInferRequest 不产出 User-Agent 头(头部为 20 项固定矩阵,无 UA);
// CLI 生产链路在 Bun 下运行,上游按 Bun UA 校验的观察来自 qodercn-gateway。
const cosyUserAgent = "Bun/1.3.14"

// inferIdentity 是组装一次 chat 请求所需的全部凭证与账号信息。
type inferIdentity struct {
	MachineID        string // 36 位 UUID
	UID              string
	Info             string // encrypt_user_info(standard Base64)
	Key              string // Cosy-Key(standard Base64)
	Version          string // cosyVersion(如 1.1.32)
	OrganizationID   string
	OrganizationTags []string
	DataPolicyAgreed bool
	Region           string // "global" | "cn"
	// PlainBody forces the plaintext variant: no body encoding, no Encode=1
	// query parameter, and the cosy signature computed over the plaintext.
	// The chat path always sends plaintext; Task 8 recordings decide the
	// final per-region constant.
	PlainBody bool
}

// preparedInfer 是可直接发起 POST 的请求形态。
type preparedInfer struct {
	URL    string
	Header http.Header
	Body   string // Encode=1 为编码串;Encode=0 为明文 JSON
}

// formatCosyUUID 输出 lowercase 8-4-4-4-12(整体格式化,不反转子字段)。
func formatCosyUUID(value [16]byte) string {
	var dst [36]byte
	hex.Encode(dst[0:8], value[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], value[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], value[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], value[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], value[10:16])
	return string(dst[:])
}

// cosyClientip 由 machineID 派生的稳定 UUID 形态值。Task 8 真机录制确认
// 双 region 的 header 矩阵均无此头;保留函数仅因 native_infer_test 曾引用,
// 若测试清理后仍无引用可删。
func cosyClientIP(machineID string) string {
	sum := md5.Sum([]byte("cosy-clientip:" + machineID))
	var u [16]byte
	copy(u[:], sum[:])
	return formatCosyUUID(u)
}

// prepareInferRequest 组装一次 chat 推理请求。clock/entropy 注入供确定性
// 测试;生产传 time.Now 与 crypto/rand(允许 nil,内部兜底)。
func (id inferIdentity) prepareInferRequest(endpoint, rawJSON, modelKey, modelSource string,
	clock func() time.Time, entropy io.Reader) (preparedInfer, error) {
	if entropy == nil {
		entropy = rand.Reader
	}
	if clock == nil {
		clock = time.Now
	}
	var random [16]byte
	if _, err := io.ReadFull(entropy, random[:]); err != nil {
		return preparedInfer{}, fmt.Errorf("read infer entropy: %w", err)
	}
	requestID := formatCosyUUID(reverseMaskUUID(random))
	unixStr := fmt.Sprintf("%d", clock().Unix())
	signedPath := normalizeSignedPath(inferPath)

	body := string(encodeNativeBody([]byte(rawJSON)))
	query := "?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	if (id.Region == "cn" && !cnEncodeBody) || id.PlainBody {
		body = rawJSON
		query = "?FetchKeys=llm_model_result&AgentId=agent_common"
	}

	payload := buildCosyPayload(id.Version, requestID, id.Info)
	payloadB64 := base64Std([]byte(payload))
	signature := cosySignature(payloadB64, id.Key, unixStr, body, signedPath)

	header := make(http.Header)
	header.Set("Accept", "text/event-stream")
	header.Set("Authorization", "Bearer COSY."+payloadB64+"."+signature)
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("Content-Type", "application/json")
	header.Set("Cosy-Business-Product", "cli")
	header.Set("Cosy-Business-Type", "agent")
	header.Set("Cosy-ClientType", "5")
	header.Set("Cosy-Data-Policy", dataPolicyValue(id.DataPolicyAgreed))
	header.Set("Cosy-Date", unixStr)
	header.Set("Cosy-Key", id.Key)
	header.Set("Cosy-Machineid", id.MachineID)
	header.Set("Cosy-Machinetoken", id.MachineID)
	header.Set("Cosy-Machinetype", "5")
	header.Set("Cosy-Scene", "assistant")
	header.Set("Cosy-User", id.UID)
	header.Set("Cosy-Version", id.Version)
	header.Set("Login-Version", "v2")
	if id.OrganizationID != "" {
		header.Set("Cosy-Organization-Id", id.OrganizationID)
	}
	if len(id.OrganizationTags) > 0 {
		header.Set("Cosy-Organization-Tags", strings.Join(id.OrganizationTags, ","))
	}
	if modelKey != "" {
		header.Set("X-Model-Key", modelKey)
		header.Set("X-Model-Source", modelSource)
	}
	// Task 8 真机录制(2026-10-05, CN, CLI 1.1.32):双 region 头矩阵一致,
	// 均 20 项;Cosy-Machinetoken == Cosy-MachineId;无 Appcode、
	// Cosy-Clientip、Cosy-Machineos、User-Agent。旧行为按 region 分叉的
	// 三个 CN 头与 Data-Policy 删除逻辑已被录制推翻,统一删除。
	return preparedInfer{
		URL:    endpoint + inferPath + query,
		Header: header,
		Body:   body,
	}, nil
}

func dataPolicyValue(agreed bool) string {
	if agreed {
		return "agree"
	}
	return "disagree"
}
