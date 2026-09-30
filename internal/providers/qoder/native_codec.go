package qoder

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
)

// qoderBodyAlphabet 是上游 CLI 对 standard base64 的 64 字符位置映射(Task 8
// 录制 fixture 后此表被真机输出交叉验证)。非机密:确定性可逆编码,非加密。
const qoderBodyAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

var qoderBodyBase64 = base64.NewEncoding(qoderBodyAlphabet).WithPadding('$').Strict()

// swapOuterThirds: k=floor(len/3),输出 C||B||A(中段吸收余数)。
func swapOuterThirds(src []byte) []byte {
	k := len(src) / 3
	out := make([]byte, 0, len(src))
	out = append(out, src[len(src)-k:]...)
	out = append(out, src[k:len(src)-k]...)
	out = append(out, src[:k]...)
	return out
}

func encodeNativeBody(raw []byte) []byte {
	encoded := make([]byte, qoderBodyBase64.EncodedLen(len(raw)))
	qoderBodyBase64.Encode(encoded, raw)
	return swapOuterThirds(encoded)
}

func decodeNativeBody(encoded []byte) ([]byte, error) {
	if bytes.IndexByte(encoded, '\r') >= 0 || bytes.IndexByte(encoded, '\n') >= 0 {
		return nil, errors.New("qoder native body invalid: line break")
	}
	b64 := swapOuterThirds(encoded)
	decoded := make([]byte, qoderBodyBase64.DecodedLen(len(b64)))
	n, err := qoderBodyBase64.Decode(decoded, b64)
	if err != nil {
		return nil, fmt.Errorf("qoder native body invalid: %w", err)
	}
	decoded = decoded[:n]
	canonical := encodeNativeBody(decoded)
	if !bytes.Equal(canonical, encoded) {
		return nil, errors.New("qoder native body invalid: not canonical")
	}
	return decoded, nil
}
