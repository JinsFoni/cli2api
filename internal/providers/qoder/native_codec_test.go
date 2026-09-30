package qoder

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestEncodeNativeBodyKnownVector(t *testing.T) {
	// 手工推导的最小向量:raw "{}" → standard base64 "e30=" → 映射后 "$#@"
	// → outer-third swap(len=4,k=1)→ 期望输出在实现后填充首例,
	// 本测试锁定:编码后再解码必须逐字节还原;以及 swap 的中段吸收规则。
	raw := []byte(`{"a":1}`)
	enc := encodeNativeBody(raw)
	if bytes.ContainsAny(enc, "+/=") {
		t.Fatalf("encoded body contains standard base64 chars: %q", enc)
	}
	dec, err := decodeNativeBody(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(dec, raw) {
		t.Fatalf("round trip mismatch: %q != %q", dec, raw)
	}
}

func TestDecodeNativeBodyRejectsNonCanonical(t *testing.T) {
	raw := []byte(`{"a":1}`)
	enc := encodeNativeBody(raw)
	// swap 两次应还原;若直接把 swap 前的中间形态喂给 decode,必须报错
	mid := swapOuterThirds(enc)
	if _, err := decodeNativeBody(mid); err == nil {
		t.Fatal("decode accepted non-canonical body")
	}
	if bytes.Equal(mid, enc) && len(mid)%3 == 0 {
		t.Fatal("swapOuterThirds identity on multiple-of-3 length; test premise broken")
	}
}

func TestEncodeNativeBodyMatchesReferenceComposition(t *testing.T) {
	// 用标准库分步复算,锁定算法组合(base64→映射→swap),防止实现走样
	raw := []byte("hello world")
	std := base64.StdEncoding.EncodeToString(raw)
	mapped := make([]byte, len(std))
	stdAlphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i, c := range []byte(std) {
		if c == '=' {
			mapped[i] = '$'
			continue
		}
		idx := bytes.IndexByte([]byte(stdAlphabet), c)
		if idx < 0 {
			t.Fatalf("unexpected std char %q", c)
		}
		mapped[i] = qoderBodyAlphabet[idx]
	}
	want := swapOuterThirds(mapped)
	if !bytes.Equal(encodeNativeBody(raw), want) {
		t.Fatalf("encode mismatch:\n got %q\nwant %q", encodeNativeBody(raw), want)
	}
}
