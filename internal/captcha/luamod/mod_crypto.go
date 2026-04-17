package luamod

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"  //nolint:gosec
	"crypto/rand"
	"crypto/sha1" //nolint:gosec
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// RegisterCrypto registers the `crypto` global table into L.
func RegisterCrypto(L *lua.LState) {
	tbl := L.NewTable()
	L.SetField(tbl, "sha256", L.NewFunction(cryptoSHA256))
	L.SetField(tbl, "sha1", L.NewFunction(cryptoSHA1))
	L.SetField(tbl, "md5", L.NewFunction(cryptoMD5))
	L.SetField(tbl, "hmac_sha256", L.NewFunction(cryptoHMACSHA256))
	L.SetField(tbl, "base64_encode", L.NewFunction(cryptoBase64Encode))
	L.SetField(tbl, "base64_decode", L.NewFunction(cryptoBase64Decode))
	L.SetField(tbl, "random_bytes", L.NewFunction(cryptoRandomBytes))
	L.SetField(tbl, "pow_solve", L.NewFunction(cryptoPowSolve))
	L.SetField(tbl, "xor", L.NewFunction(cryptoXOR))
	L.SetField(tbl, "aes_encrypt", L.NewFunction(cryptoAESEncrypt))
	L.SetField(tbl, "aes_decrypt", L.NewFunction(cryptoAESDecrypt))
	L.SetGlobal("crypto", tbl)
}

// cryptoSHA256 implements crypto.sha256(data) → hex string.
func cryptoSHA256(L *lua.LState) int {
	data := L.CheckString(1)
	h := sha256.Sum256([]byte(data))
	L.Push(lua.LString(hex.EncodeToString(h[:])))
	return 1
}

// cryptoSHA1 implements crypto.sha1(data) → hex string.
func cryptoSHA1(L *lua.LState) int {
	data := L.CheckString(1)
	h := sha1.Sum([]byte(data)) //nolint:gosec
	L.Push(lua.LString(hex.EncodeToString(h[:])))
	return 1
}

// cryptoMD5 implements crypto.md5(data) → hex string.
func cryptoMD5(L *lua.LState) int {
	data := L.CheckString(1)
	h := md5.Sum([]byte(data)) //nolint:gosec
	L.Push(lua.LString(hex.EncodeToString(h[:])))
	return 1
}

// cryptoHMACSHA256 implements crypto.hmac_sha256(key, data) → hex string.
func cryptoHMACSHA256(L *lua.LState) int {
	key := L.CheckString(1)
	data := L.CheckString(2)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(data))
	L.Push(lua.LString(hex.EncodeToString(mac.Sum(nil))))
	return 1
}

// cryptoBase64Encode implements crypto.base64_encode(data) → string.
func cryptoBase64Encode(L *lua.LState) int {
	data := L.CheckString(1)
	L.Push(lua.LString(base64.StdEncoding.EncodeToString([]byte(data))))
	return 1
}

// cryptoBase64Decode implements crypto.base64_decode(str) → data string.
func cryptoBase64Decode(L *lua.LState) int {
	str := L.CheckString(1)
	data, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		L.RaiseError("crypto.base64_decode: %v", err)
		return 0
	}
	L.Push(lua.LString(data))
	return 1
}

// cryptoRandomBytes implements crypto.random_bytes(n) → raw bytes string.
func cryptoRandomBytes(L *lua.LState) int {
	n := L.CheckInt(1)
	if n < 0 {
		L.RaiseError("crypto.random_bytes: n must be non-negative")
		return 0
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		L.RaiseError("crypto.random_bytes: %v", err)
		return 0
	}
	L.Push(lua.LString(buf))
	return 1
}

const powMaxIterations = 100_000_000

// cryptoPowSolve implements crypto.pow_solve(prefix, difficulty) → hash string.
// Matches Go computeProofOfWork exactly: nonce is a decimal integer (1,2,3...),
// finds sha256(prefix + nonce_decimal) with `difficulty` leading HEX zeros,
// returns the hex hash string (not the nonce).
func cryptoPowSolve(L *lua.LState) int {
	prefix := L.CheckString(1)
	difficulty := L.CheckInt(2)
	if difficulty < 0 || difficulty > 64 {
		L.RaiseError("crypto.pow_solve: difficulty must be 0..64 (hex zeros)")
		return 0
	}
	if prefix == "" || difficulty == 0 {
		L.Push(lua.LString(""))
		return 1
	}

	hexPrefix := strings.Repeat("0", difficulty)

	for nonce := 1; nonce < powMaxIterations; nonce++ {
		data := prefix + strconv.Itoa(nonce)
		h := sha256.Sum256([]byte(data))
		hexStr := hex.EncodeToString(h[:])
		if strings.HasPrefix(hexStr, hexPrefix) {
			L.Push(lua.LString(hexStr))
			return 1
		}
	}

	L.RaiseError("crypto.pow_solve: not found within %d iterations", powMaxIterations)
	return 0
}

// cryptoXOR implements crypto.xor(data1, data2) → string.
// Byte-wise XOR; the shorter input repeats to match the longer.
func cryptoXOR(L *lua.LState) int {
	a := []byte(L.CheckString(1))
	b := []byte(L.CheckString(2))
	if len(a) == 0 || len(b) == 0 {
		L.Push(lua.LString(""))
		return 1
	}
	longer := len(a)
	if len(b) > longer {
		longer = len(b)
	}
	out := make([]byte, longer)
	for i := range out {
		out[i] = a[i%len(a)] ^ b[i%len(b)]
	}
	L.Push(lua.LString(out))
	return 1
}

// pkcs7Pad pads plaintext to a multiple of blockSize using PKCS7.
func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	padding := bytes.Repeat([]byte{byte(pad)}, pad)
	return append(data, padding...)
}

// pkcs7Unpad removes PKCS7 padding.
func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, fmt.Errorf("invalid data length %d", len(data))
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > blockSize {
		return nil, fmt.Errorf("invalid padding value %d", pad)
	}
	for _, v := range data[len(data)-pad:] {
		if int(v) != pad {
			return nil, fmt.Errorf("invalid padding byte %d", v)
		}
	}
	return data[:len(data)-pad], nil
}

// cryptoAESEncrypt implements crypto.aes_encrypt(key, iv, plaintext) → base64 string.
// AES-128-CBC with PKCS7 padding; key and iv must be 16 bytes.
func cryptoAESEncrypt(L *lua.LState) int {
	key := []byte(L.CheckString(1))
	iv := []byte(L.CheckString(2))
	plaintext := []byte(L.CheckString(3))

	block, err := aes.NewCipher(key)
	if err != nil {
		L.RaiseError("crypto.aes_encrypt: %v", err)
		return 0
	}
	if len(iv) != aes.BlockSize {
		L.RaiseError("crypto.aes_encrypt: iv must be %d bytes", aes.BlockSize)
		return 0
	}

	padded := pkcs7Pad(plaintext, aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	L.Push(lua.LString(base64.StdEncoding.EncodeToString(ciphertext)))
	return 1
}

// cryptoAESDecrypt implements crypto.aes_decrypt(key, iv, ciphertext_base64) → plaintext string.
func cryptoAESDecrypt(L *lua.LState) int {
	key := []byte(L.CheckString(1))
	iv := []byte(L.CheckString(2))
	ciphertextB64 := L.CheckString(3)

	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		L.RaiseError("crypto.aes_decrypt: base64 decode: %v", err)
		return 0
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		L.RaiseError("crypto.aes_decrypt: %v", err)
		return 0
	}
	if len(iv) != aes.BlockSize {
		L.RaiseError("crypto.aes_decrypt: iv must be %d bytes", aes.BlockSize)
		return 0
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		L.RaiseError("crypto.aes_decrypt: ciphertext length %d is not a multiple of block size", len(ciphertext))
		return 0
	}

	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)

	unpadded, err := pkcs7Unpad(plaintext, aes.BlockSize)
	if err != nil {
		L.RaiseError("crypto.aes_decrypt: unpad: %v", err)
		return 0
	}

	L.Push(lua.LString(unpadded))
	return 1
}
