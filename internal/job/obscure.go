package job

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
)

// obscureKey is the fixed key that rclone uses to obscure passwords in its
// config file. It is public: obscured is not encrypted.
var obscureKey = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// reveal decodes a password that rclone obscured (AES-CTR, IV first,
// URL-safe base64 with no padding). hdup decodes it in the process, so the
// value is never an argument of a command.
func reveal(obscured string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(obscured)
	if err != nil || len(b) < aes.BlockSize {
		return "", errors.New("the value is not an obscured rclone password")
	}
	block, err := aes.NewCipher(obscureKey)
	if err != nil {
		return "", err
	}
	iv, data := b[:aes.BlockSize], b[aes.BlockSize:]
	cipher.NewCTR(block, iv).XORKeyStream(data, data)
	return string(data), nil
}
