// Credentials of the hub's one user (work order C11b section 1): an
// argon2id password hash, an optional TOTP secret (RFC 6238, SHA-1, six
// digits, 30-second steps), and the random tokens of login sessions, action
// buttons and enrolments. Tokens are 256-bit random values handed out once
// and stored only as their SHA-256; every comparison of a secret is
// constant-time.

package hub

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (RFC 9106, second recommended option).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// MinPasswordLength is the shortest password set-password accepts.
const MinPasswordLength = 12

// MaxPasswordLength bounds what is hashed (a login body is small anyway).
const MaxPasswordLength = 1024

// HashPassword returns the PHC string of an argon2id hash of password.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

var phcRE = regexp.MustCompile(`^\$argon2id\$v=(\d+)\$m=(\d+),t=(\d+),p=(\d+)\$([A-Za-z0-9+/]+)\$([A-Za-z0-9+/]+)$`)

// VerifyPassword reports whether password matches the PHC string hash. A
// malformed hash never matches.
func VerifyPassword(hash, password string) bool {
	m := phcRE.FindStringSubmatch(hash)
	if m == nil || len(password) > MaxPasswordLength {
		return false
	}
	v, _ := strconv.Atoi(m[1])
	mem, err1 := strconv.ParseUint(m[2], 10, 32)
	t, err2 := strconv.ParseUint(m[3], 10, 32)
	p, err3 := strconv.ParseUint(m[4], 10, 8)
	// Bounds keep a tampered database from making a login take forever.
	if v != argon2.Version || err1 != nil || err2 != nil || err3 != nil || mem < 8 || mem > 1<<20 || t < 1 || t > 20 || p < 1 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(m[5])
	if err != nil {
		return false
	}
	want, err := enc.DecodeString(m[6])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, uint32(t), uint32(mem), uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// TOTP parameters.
const (
	totpStep   = 30 * time.Second
	totpDigits = 6
	// totpSkew is how many steps before and after the present are accepted.
	totpSkew = 1
	// totpSecretLen is 160 bits, as RFC 4226 recommends.
	totpSecretLen = 20
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a new secret in base32 (as authenticator apps take
// it).
func NewTOTPSecret() (string, error) {
	b := make([]byte, totpSecretLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// TOTPURI is the otpauth:// URI an authenticator app scans.
func TOTPURI(secret, account string) string {
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", "devc")
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(totpDigits))
	q.Set("period", strconv.Itoa(int(totpStep/time.Second)))
	return "otpauth://totp/" + url.PathEscape("devc:"+account) + "?" + q.Encode()
}

// hotp is RFC 4226 with SHA-1 and digits digits.
func hotp(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod)
}

// TOTPCode is the code of secret at t (what an authenticator app shows).
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", err
	}
	return hotp(key, uint64(totpStepAt(t)), totpDigits), nil
}

// totpStepAt is the time step of t.
func totpStepAt(t time.Time) int64 { return t.Unix() / int64(totpStep/time.Second) }

// CheckTOTP verifies a code against secret at now, allowing one step of
// clock skew each way. It returns the step that matched; a step at or
// below lastStep (a code used before) does not count, so a code works once.
func CheckTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return 0, false
	}
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	cur := totpStepAt(now)
	matched, found := int64(0), 0
	for d := -totpSkew; d <= totpSkew; d++ {
		step := cur + int64(d)
		if step <= lastStep || step < 0 {
			continue
		}
		// Every candidate is compared, so the time taken does not say which
		// one matched.
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(step), totpDigits)), []byte(code)) == 1 {
			matched, found = step, 1
		}
	}
	return matched, found == 1
}

// TokenBytes is the size of every random token: 256 bits.
const TokenBytes = 32

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// NewToken returns a random 256-bit token (base64url, 43 characters) and
// the ID it is stored under.
func NewToken() (token, id string, err error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, TokenID(token), nil
}

// ValidToken reports whether s has the form of a token.
func ValidToken(s string) bool { return tokenRE.MatchString(s) }

// TokenID is the hex SHA-256 of a token: what the database holds. Looking
// a token up by its hash reveals nothing about the token through timing.
func TokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sameID compares two token IDs in constant time.
func sameID(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

var errNoPassword = errors.New("no password is set: run 'devc hub user set-password' on the hub")
