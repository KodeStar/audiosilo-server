package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. Tuned for an interactive login on modest self-hosted
// hardware; raise memory/time if you run on something beefier.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB => 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword returns an argon2id PHC-style encoded hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the encoded argon2id hash.
// Comparison is constant time.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, err
	}
	var mem, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, time, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// generateToken returns a URL-safe random secret and its storage hash.
func generateToken() (secret, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	secret = base64.RawURLEncoding.EncodeToString(buf)
	return secret, hashSecret(secret), nil
}

// codeEncoding uses Crockford's base32 alphabet (excludes I, L, O, U), which is
// designed to be unambiguous for humans to read and type. normalizeCode maps
// the common look-alikes back so users can fat-finger O/I/L and still succeed.
var codeEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// generateAuthCode returns a human-typable auth code (groups of 4) and its hash.
func generateAuthCode() (code, hash string, err error) {
	buf := make([]byte, 10) // 16 base32 chars
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	raw := codeEncoding.EncodeToString(buf)
	var b strings.Builder
	for i, r := range raw {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	code = b.String()
	return code, hashSecret(normalizeCode(code)), nil
}

// normalizeCode strips formatting and maps Crockford look-alike characters so
// codes verify regardless of spacing, case, or O/0 and I/L/1 confusion.
func normalizeCode(code string) string {
	up := strings.ToUpper(strings.TrimSpace(code))
	return strings.NewReplacer(
		"-", "", " ", "",
		"O", "0", "I", "1", "L", "1", "U", "V",
	).Replace(up)
}

// hashSecret hashes a presented secret for constant-effort DB lookup. Tokens
// and codes have full entropy, so a fast hash (SHA-256) is appropriate here —
// argon2id is reserved for low-entropy user passwords.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
