// This file generates, validates and decodes session IDs.

package names

import (
	"crypto/rand"
	"encoding/binary"
	"regexp"
	"sync"
	"time"
)

// Session IDs are "s" followed by 20 characters of lower-case base32hex
// (0-9, a-v). base32hex is used rather than the RFC 4648 base32 alphabet
// because its ASCII order equals its numeric order, which makes lexical order
// equal creation order. The 100 bits are a 50-bit millisecond timestamp
// followed by 50 random bits.
const (
	sessionAlphabet = "0123456789abcdefghijklmnopqrstuv"
	sessionChars    = 20
	sessionTimeBits = 50
	sessionRandBits = 50
)

var sessionRE = regexp.MustCompile(`^s[0-9a-v]{20}$`)

// ValidSessionID reports whether s has the form produced by NewSessionID.
func ValidSessionID(s string) bool {
	return sessionRE.MatchString(s)
}

var sessionGen = struct {
	sync.Mutex
	lastMS   uint64
	lastRand uint64
}{}

// NewSessionID returns a new, time-ordered session ID. Within one process the
// IDs are strictly increasing even when several are created in the same
// millisecond.
func NewSessionID() string {
	return newSessionID(time.Now())
}

func newSessionID(now time.Time) string {
	const randMask = uint64(1)<<sessionRandBits - 1
	ms := uint64(now.UnixMilli()) & (uint64(1)<<sessionTimeBits - 1)

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Linux; a panic is better than a guessable ID.
		panic("names: reading random bytes: " + err.Error())
	}
	r := binary.BigEndian.Uint64(b[:]) & randMask

	sessionGen.Lock()
	if ms <= sessionGen.lastMS {
		// Same millisecond (or the clock went backwards): continue the
		// previous sequence so ordering holds.
		ms = sessionGen.lastMS
		r = sessionGen.lastRand + 1
		if r > randMask {
			ms++
			r = 0
		}
	}
	sessionGen.lastMS, sessionGen.lastRand = ms, r
	sessionGen.Unlock()

	return encodeSession(ms, r)
}

func encodeSession(ms, r uint64) string {
	var out [1 + sessionChars]byte
	out[0] = 's'
	for i := sessionChars / 2; i >= 1; i-- {
		out[i] = sessionAlphabet[ms&31]
		ms >>= 5
	}
	for i := sessionChars; i > sessionChars/2; i-- {
		out[i] = sessionAlphabet[r&31]
		r >>= 5
	}
	return string(out[:])
}

// SessionTime returns the creation time encoded in a valid session ID.
func SessionTime(id string) (time.Time, bool) {
	if !ValidSessionID(id) {
		return time.Time{}, false
	}
	var ms uint64
	for i := 1; i <= sessionChars/2; i++ {
		c := id[i]
		var v byte
		if c <= '9' {
			v = c - '0'
		} else {
			v = c - 'a' + 10
		}
		ms = ms<<5 | uint64(v)
	}
	return time.UnixMilli(int64(ms)), true
}
