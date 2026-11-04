package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP minimum: 19 MiB, 2 passes, 1 thread).
const (
	argonMemoryKiB = 19456
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
)

var errBadHash = errors.New("password hash is not argon2id PHC")

// hashSlots caps the hashes that run at the same time, because each one
// takes 19 MiB of memory.
var hashSlots = make(chan struct{}, runtime.NumCPU())

// HashPassword returns an argon2id hash in PHC string format.
func HashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	rand.Read(salt)
	return hashWithSalt(password, salt)
}

func hashWithSalt(password string, salt []byte) string {
	key := idKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// CheckPassword reports if password matches the PHC hash. It reads the
// parameters from the hash, so hashes with earlier parameters still work.
func CheckPassword(hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, errBadHash
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil || memory == 0 || time == 0 || threads == 0 {
		return false, errBadHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, errBadHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) < 16 {
		return false, errBadHash
	}
	got := idKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func idKey(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey(password, salt, time, memory, threads, keyLen)
}

// dummyHash lets a login for an unknown email take as long as a wrong password.
var dummyHash = sync.OnceValue(func() string { return HashPassword("runplane dummy password") })
