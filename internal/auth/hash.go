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
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. Tuned for an interactive login on modest self-hosted
// hardware; raise memory/time if you run on something beefier. Memory (64 MiB)
// comfortably exceeds the OWASP floor; time=2 is cheap defense-in-depth.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // KiB => 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

type argonCost struct {
	time, memory uint32
	threads      uint8
}

// realCost is the cost above; hashCost is what HashPassword spends, lowered only
// by UseCheapHashingForTests. VerifyPassword reads the cost from the hash itself.
var (
	realCost = argonCost{argonTime, argonMemory, argonThreads}
	hashCost = realCost
)

// HashPassword returns an argon2id PHC-style encoded hash.
func HashPassword(password string) (string, error) {
	return hashWithCost(password, hashCost)
}

func hashWithCost(password string, c argonCost) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argonKey([]byte(password), salt, c.time, c.memory, c.threads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, c.memory, c.time, c.threads,
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
	var mem, passes uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &passes, &threads); err != nil {
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
	got := argonKey([]byte(password), salt, passes, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// argonKey is argon2.IDKey followed by handing its work memory back to the OS
// (hashMemory). Every argon2id hash and verify goes through it.
func argonKey(password, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
	key := argon2.IDKey(password, salt, passes, memory, threads, keyLen)
	hashMemory.release()
	return key
}

// hashReleaseDelay is how long the hashes must have been quiet before their
// memory is released, so a burst of sign-ins costs one release, not one each.
const hashReleaseDelay = time.Second

// hashMemory is the release after the last hash (see memoryReleaser).
var hashMemory = &memoryReleaser{delay: hashReleaseDelay, free: debug.FreeOSMemory}

// memoryReleaser hands an argon2id hash's work memory (argonMemory, 64 MiB, a
// fresh allocation each time) back to the OS once the hashes go quiet. Without
// it a server that hashes a password or two at start (the first-run admin, a
// sign-in) and then idles keeps that memory: the heap grew to hold the block, so
// the garbage it leaves stays under the next GC's goal and no GC runs to free it,
// and the background scavenger returns freed pages only slowly. An idle server
// with an empty library sat at about 140 MiB instead of about 20 (server#244).
// The release (debug.FreeOSMemory: a GC, then every free page returned) runs on
// its own goroutine after delay with no further hash, never on the request's.
type memoryReleaser struct {
	delay time.Duration
	free  func()

	mu    sync.Mutex
	timer *time.Timer
}

// release schedules the release delay from now, pushing back one already due.
func (r *memoryReleaser) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer == nil {
		r.timer = time.AfterFunc(r.delay, r.free)
		return
	}
	r.timer.Reset(r.delay)
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
// and codes have full entropy, so a fast hash (SHA-256) is appropriate here -
// argon2id is reserved for low-entropy user passwords.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
