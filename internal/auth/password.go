package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    uint32 = 1
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"argon2id$%d$%d$%d$%s$%s",
		argonTime,
		argonMemory,
		argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

func VerifyPassword(encodedHash, password string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[0] != "argon2id" {
		return false, fmt.Errorf("invalid hash format")
	}

	timeCost, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return false, fmt.Errorf("parse time cost: %w", err)
	}
	memoryCost, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return false, fmt.Errorf("parse memory cost: %w", err)
	}
	threads, err := strconv.ParseUint(parts[3], 10, 8)
	if err != nil {
		return false, fmt.Errorf("parse thread count: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("decode salt: %w", err)
	}
	expectedHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("decode hash: %w", err)
	}

	actualHash := argon2.IDKey(
		[]byte(password),
		salt,
		uint32(timeCost),
		uint32(memoryCost),
		uint8(threads),
		uint32(len(expectedHash)),
	)

	return subtle.ConstantTimeCompare(expectedHash, actualHash) == 1, nil
}
