package qoder

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// mustJSONMarshal 对 string 调用 json.Marshal;入参是 string,失败不可能发生。
func mustJSONMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// jsonString 输出与 encoding/json 相容的带引号字符串(转义规则一致)。
func jsonString(s string) string {
	return string(mustJSONMarshal(s))
}

// buildCosyPayload 固定字段顺序的 raw JSON(字段顺序影响签名,禁止 map)。
func buildCosyPayload(version, requestID, info string) string {
	return fmt.Sprintf(`{"version":"v1","requestId":%s,"info":%s,"cosyVersion":%s,"ideVersion":""}`,
		jsonString(requestID), jsonString(info), jsonString(version))
}

func cosySignature(payloadB64, key, unixSeconds, encodedBody, signedPath string) string {
	preimage := payloadB64 + "\n" + key + "\n" + unixSeconds + "\n" + encodedBody + "\n" + signedPath
	sum := md5.Sum([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

func normalizeSignedPath(path string) string {
	return strings.TrimPrefix(path, "/algo")
}
