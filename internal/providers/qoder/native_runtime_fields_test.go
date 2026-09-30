package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestGenerateRuntimeFieldsRejectsNilTags(t *testing.T) {
	if _, err := generateRuntimeFields(runtimeFieldsInput{UID: "u", OrganizationTags: nil}, rand.Reader); err == nil {
		t.Fatal("nil tags must fail closed")
	}
}

func TestGenerateRuntimeFieldsAcceptsEmptyTags(t *testing.T) {
	if _, err := generateRuntimeFields(runtimeFieldsInput{UID: "u", OrganizationTags: []string{}}, rand.Reader); err != nil {
		t.Fatalf("empty slice is valid upstream: %v", err)
	}
}

func TestGenerateRuntimeFieldsDeterministic(t *testing.T) {
	in := runtimeFieldsInput{UID: "synthetic-user", OrganizationID: "synthetic-org",
		OrganizationTags: []string{"a", "b"}, DataPolicyAgreed: true}
	entropy := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18}, 10)
	a, err := generateRuntimeFields(in, bytes.NewReader(entropy))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := generateRuntimeFields(in, bytes.NewReader(entropy))
	if a != b {
		t.Fatal("same entropy must yield same output")
	}
	// 结构校验:公钥 2048-bit → RSA 密文 256 字节,标准 Base64。
	infoRaw, err := base64.StdEncoding.DecodeString(a.EncryptUserInfo)
	if err != nil {
		t.Fatalf("encrypt_user_info not standard base64: %v", err)
	}
	keyRaw, err := base64.StdEncoding.DecodeString(a.Key)
	if err != nil {
		t.Fatalf("key not standard base64: %v", err)
	}
	if len(keyRaw) != 256 {
		t.Fatalf("RSA ciphertext must be 256 bytes, got %d", len(keyRaw))
	}
	if len(infoRaw)%16 != 0 {
		t.Fatal("AES ciphertext not block aligned")
	}
	// 解密回环:用同一熵流复算 AES key(reverse+mask+hex8),解开
	// encrypt_user_info 应得到输入 JSON,锁定 key 派生与 AES 参数。
	var random [16]byte
	copy(random[:], entropy[:16])
	expectedKey := runtimeASCIIKey(reverseMaskUUID(random))
	block, err := aes.NewCipher(expectedKey)
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, len(infoRaw))
	cipher.NewCBCDecrypter(block, expectedKey).CryptBlocks(plain, infoRaw)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize {
		t.Fatalf("invalid PKCS7 pad %d", pad)
	}
	plain = plain[:len(plain)-pad]
	want, _ := json.Marshal(struct {
		UID              string   `json:"uid"`
		OrganizationID   string   `json:"organization_id"`
		OrganizationTags []string `json:"organization_tags"`
		DataPolicyAgreed bool     `json:"data_policy_agreed"`
	}{in.UID, in.OrganizationID, in.OrganizationTags, in.DataPolicyAgreed})
	if !bytes.Equal(plain, want) {
		t.Fatalf("decrypted userinfo mismatch:\n got %s\nwant %s", plain, want)
	}
}

func TestReverseMaskUUIDFormat(t *testing.T) {
	// 全 0 熵反转后仍是全 0,掩码后应为 00000000-0000-4000-8000-000000000000
	var random [16]byte
	u := reverseMaskUUID(random)
	if u[6] != 0x40 {
		t.Fatalf("version nibble: %#x", u[6])
	}
	if u[8] != 0x80 {
		t.Fatalf("variant bits: %#x", u[8])
	}
	// 非对称熵:第 1 字节反转后应出现在最后
	var r2 [16]byte
	r2[0] = 0xAB
	u2 := reverseMaskUUID(r2)
	if u2[15] != 0xAB {
		t.Fatalf("reversal mismatch: u2[15]=%#x", u2[15])
	}
}
