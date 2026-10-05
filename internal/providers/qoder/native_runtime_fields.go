package qoder

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"sync"
)

// runtimePublicKeyPEM 是上游 CLI 用于生成 Cosy-Key 的固定公钥(纯公钥材料,
// 非上游版权代码)。Task 11 真机录制推翻了最初提取:WASM 内嵌两把公钥,
// generate_runtime_auth_fields 实际使用的是这把 1024-bit 密钥(密文 128 字节
// → base64 172 字符,与真实 CN 账号持久化的 .auth/user 中 key 长度一致);
// 2048-bit 那把(先前蒸馏的来源)用于其他用途。两把均为纯公钥材料。
const runtimePublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// runtimeFields 是 chat 请求头需要的一对运行时字段。
type runtimeFields struct {
	EncryptUserInfo string // standard Base64(AES-128-CBC(userinfo JSON))
	Key             string // standard Base64(RSA-PKCS1v15(AES key))
}

// runtimeFieldsInput 是加密前的 userinfo JSON 字段,顺序固定
// (uid, organization_id, organization_tags, data_policy_agreed),
// 上游 WASM 按此顺序序列化;字段顺序影响密文但不影响签名。
type runtimeFieldsInput struct {
	UID              string
	OrganizationID   string
	OrganizationTags []string // nil 非法;空 slice 合法(上游语义)
	DataPolicyAgreed bool
}

// reverseMaskUUID 把 16 熵字节整段反转后打上 UUID v4 版本位。反转是上游
// WASM 熵处理的第一步;版本位保证产物形如合法 UUID。
func reverseMaskUUID(random [16]byte) [16]byte {
	var u [16]byte
	for i := range u {
		u[i] = random[15-i]
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

// runtimeASCIIKey 取 UUID 前 8 字节的 lowercase hex(16 个 ASCII 字符),
// 直接用作 AES-128-CBC 的 key 与 IV。
func runtimeASCIIKey(value [16]byte) []byte {
	var key [16]byte
	hex.Encode(key[:], value[:8])
	return key[:]
}

// generateRuntimeFields 为一个凭证生成 encrypt_user_info 与 Cosy-Key。
// entropy 注入保证确定性测试;生产传 nil(用 crypto/rand)。
func generateRuntimeFields(input runtimeFieldsInput, entropy io.Reader) (runtimeFields, error) {
	if input.OrganizationTags == nil {
		return runtimeFields{}, errors.New("qoder runtime fields: nil organization tags")
	}
	// 注入熵时 RSA 填充也走确定性派生;必须在下面的 crypto/rand 回退前判定。
	deterministicRSA := entropy != nil
	if entropy == nil {
		entropy = rand.Reader
	}
	raw, err := json.Marshal(struct {
		UID              string   `json:"uid"`
		OrganizationID   string   `json:"organization_id"`
		OrganizationTags []string `json:"organization_tags"`
		DataPolicyAgreed bool     `json:"data_policy_agreed"`
	}{input.UID, input.OrganizationID, input.OrganizationTags, input.DataPolicyAgreed})
	if err != nil {
		return runtimeFields{}, fmt.Errorf("qoder runtime fields: marshal runtime userinfo: %w", err)
	}
	var random [16]byte
	if _, err := io.ReadFull(entropy, random[:]); err != nil {
		return runtimeFields{}, fmt.Errorf("qoder runtime fields: read runtime uuid entropy: %w", err)
	}
	uuid := reverseMaskUUID(random)
	key := runtimeASCIIKey(uuid)
	ciphertext, err := aesCBCEncryptPKCS7(raw, key, key)
	if err != nil {
		return runtimeFields{}, fmt.Errorf("qoder runtime fields: aes encrypt userinfo: %w", err)
	}
	publicKey, err := parseRuntimePublicKey()
	if err != nil {
		return runtimeFields{}, fmt.Errorf("qoder runtime fields: load public key: %w", err)
	}
	// crypto/rsa 保证 PS 无 0 字节(内部对 0 字节重抽),满足上游
	// nonZeroRandomBytes 语义。RSA 填充随机源:注入熵时以 16 个熵字节
	// 作种子的确定性计数器读取器,保证可复现密文;nil 熵(生产)保持
	// crypto/rand。
	rsaRandom := rand.Reader
	if deterministicRSA {
		rsaRandom = newDeterministicReader(random[:])
	}
	encryptedKey, err := rsa.EncryptPKCS1v15(rsaRandom, publicKey, key)
	if err != nil {
		return runtimeFields{}, fmt.Errorf("qoder runtime fields: rsa encrypt key: %w", err)
	}
	return runtimeFields{
		EncryptUserInfo: base64Std(ciphertext),
		Key:             base64Std(encryptedKey),
	}, nil
}

// deterministicReader 为 RSA 填充提供可复现随机字节:每次 Read 的内容只
// 依赖 seed 与本次请求的长度(块计数器按长度键控)。之所以不做成跨调用
// 的流式计数器,是因为 crypto/rsa 在读取填充前会经 randutil.MaybeReadByte
// 以约 50% 概率额外探测消费 1 字节,任何依赖调用历史的流都会因此错位,
// 密文不可复现;按长度键控对探测免疫(与 stdlib zeroReader 同一模式)。
// 仅用于测试注入;生产传 nil 熵时走 crypto/rand。
type deterministicReader struct {
	seed []byte
}

func newDeterministicReader(seed []byte) *deterministicReader {
	return &deterministicReader{seed: seed}
}

func (r *deterministicReader) Read(p []byte) (int, error) {
	filled := 0
	for counter := uint64(0); filled < len(p); counter++ {
		var tag [16]byte
		binary.LittleEndian.PutUint64(tag[:8], uint64(len(p)))
		binary.LittleEndian.PutUint64(tag[8:], counter)
		h := sha256.New()
		h.Write(r.seed)
		h.Write(tag[:])
		filled += copy(p[filled:], h.Sum(nil))
	}
	return len(p), nil
}

// base64Std 是上游运行时字段使用的标准 Base64(带 padding)编码。
func base64Std(src []byte) string {
	return base64.StdEncoding.EncodeToString(src)
}

// runtimePublicKeyOnce/缓存保证内嵌 PEM 只解析一次:generateRuntimeFields
// 每次调用都会经过 parseRuntimePublicKey(热路径),而 PEM 是编译期常量,
// 解析结果不变,因此用 sync.Once 求值一次并缓存公钥与错误。
var (
	runtimePublicKeyOnce sync.Once
	runtimePublicKey     *rsa.PublicKey
	runtimePublicKeyErr  error
)

// parseRuntimePublicKey 返回内嵌 SPKI PEM 解析出的 RSA 公钥;首次调用时
// 解析,之后直接命中缓存。
func parseRuntimePublicKey() (*rsa.PublicKey, error) {
	runtimePublicKeyOnce.Do(func() {
		runtimePublicKey, runtimePublicKeyErr = decodeRuntimePublicKey()
	})
	return runtimePublicKey, runtimePublicKeyErr
}

// decodeRuntimePublicKey 解析内嵌 PEM;仅由 parseRuntimePublicKey 经
// sync.Once 调用一次。
func decodeRuntimePublicKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(runtimePublicKeyPEM))
	if block == nil {
		return nil, errors.New("qoder runtime fields: invalid embedded public key PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("qoder runtime fields: parse public key: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("qoder runtime fields: embedded key is not RSA")
	}
	return key, nil
}

// aesCBCEncryptPKCS7 做 PKCS7 填充的 AES-CBC 加密;iv 必须与块长一致。
func aesCBCEncryptPKCS7(plaintext, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("qoder runtime fields: aes cipher: %w", err)
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, len(plaintext)+pad)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out, nil
}
