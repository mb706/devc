// Package display holds the wire formats of the desktop sidecar (work order
// C19): Xauthority files, the X11 connection setup (enough to prove that a
// display accepts or refuses a client), and the password file and challenge
// response of VNC authentication. It is a leaf package shared by the host
// (internal/gui), the box (`devc box xcheck`) and the tests.
//
// Security rules: none of its own. Parsers are strict and bounded, since
// the box side reads files the box can write and answers from servers the
// box can run.
package display

import (
	"bytes"
	"crypto/des"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Xauthority address families.
const (
	FamilyInternet = 0
	FamilyLocal    = 256
	FamilyWild     = 0xFFFF
)

// CookieName is the only authorisation protocol devc uses.
const CookieName = "MIT-MAGIC-COOKIE-1"

// CookieLen is the length of an MIT-MAGIC-COOKIE-1 cookie.
const CookieLen = 16

// XAuthEntry is one entry of an Xauthority file.
type XAuthEntry struct {
	Family  uint16
	Address []byte
	Number  string // display number
	Name    string // authorisation protocol
	Data    []byte
}

// EncodeXAuthority renders entries in the Xauthority file format (every
// field a big-endian 16-bit length and its bytes).
func EncodeXAuthority(entries []XAuthEntry) ([]byte, error) {
	var b bytes.Buffer
	field := func(p []byte) error {
		if len(p) > 0xFFFF {
			return errors.New("xauthority field too long")
		}
		_ = binary.Write(&b, binary.BigEndian, uint16(len(p)))
		b.Write(p)
		return nil
	}
	for _, e := range entries {
		_ = binary.Write(&b, binary.BigEndian, e.Family)
		for _, p := range [][]byte{e.Address, []byte(e.Number), []byte(e.Name), e.Data} {
			if err := field(p); err != nil {
				return nil, err
			}
		}
	}
	return b.Bytes(), nil
}

// CookieFile is the Xauthority file of a display: one entry of family
// "wild", so a client finds it under any name of the display (":1",
// "127.0.0.1:1", "localhost:1").
func CookieFile(cookie []byte, number int) ([]byte, error) {
	if len(cookie) != CookieLen {
		return nil, fmt.Errorf("an X cookie has %d bytes, not %d", CookieLen, len(cookie))
	}
	return EncodeXAuthority([]XAuthEntry{{Family: FamilyWild, Number: strconv.Itoa(number), Name: CookieName, Data: cookie}})
}

// MaxXAuthority bounds the Xauthority files ParseXAuthority accepts.
const MaxXAuthority = 64 << 10

// ParseXAuthority parses an Xauthority file; a truncated entry is an error.
func ParseXAuthority(b []byte) ([]XAuthEntry, error) {
	if len(b) > MaxXAuthority {
		return nil, fmt.Errorf("xauthority file larger than %d bytes", MaxXAuthority)
	}
	r := bytes.NewReader(b)
	field := func() ([]byte, error) {
		var n uint16
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			return nil, err
		}
		return p, nil
	}
	var out []XAuthEntry
	for r.Len() > 0 {
		var e XAuthEntry
		if err := binary.Read(r, binary.BigEndian, &e.Family); err != nil {
			return nil, errors.New("truncated xauthority entry")
		}
		var fs [4][]byte
		for i := range fs {
			p, err := field()
			if err != nil {
				return nil, errors.New("truncated xauthority entry")
			}
			fs[i] = p
		}
		e.Address, e.Number, e.Name, e.Data = fs[0], string(fs[1]), string(fs[2]), fs[3]
		out = append(out, e)
	}
	return out, nil
}

// FindCookie returns the MIT-MAGIC-COOKIE-1 data for display number on a
// TCP connection to the IPv4 address addr (nil for a local connection):
// the first entry of family wild, of family internet with that address, or
// of family local for a local connection.
func FindCookie(entries []XAuthEntry, number int, addr []byte) ([]byte, bool) {
	n := strconv.Itoa(number)
	for _, e := range entries {
		if e.Number != n || e.Name != CookieName {
			continue
		}
		switch {
		case e.Family == FamilyWild,
			e.Family == FamilyInternet && addr != nil && bytes.Equal(e.Address, addr),
			e.Family == FamilyLocal && addr == nil:
			return e.Data, true
		}
	}
	return nil, false
}

// Setup is what a display tells an accepted client.
type Setup struct {
	Vendor        string
	Release       uint32
	Width, Height uint16 // of the first screen
}

// RefusedError is a display's refusal of the connection setup.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string {
	if e.Reason == "" {
		return "the display refused the connection"
	}
	return "the display refused the connection: " + e.Reason
}

// MaxSetupReply bounds the setup reply Handshake reads.
const MaxSetupReply = 1 << 20

func pad4(n int) int { return (4 - n%4) % 4 }

// Handshake performs the X11 connection setup on c with the given
// authorisation (empty name and data for none) and reads the answer. A
// refusal is a *RefusedError.
func Handshake(c io.ReadWriter, authName string, authData []byte) (*Setup, error) {
	var req bytes.Buffer
	req.WriteByte('l') // little-endian
	req.WriteByte(0)
	_ = binary.Write(&req, binary.LittleEndian, [3]uint16{11, 0, uint16(len(authName))})
	_ = binary.Write(&req, binary.LittleEndian, uint16(len(authData)))
	req.Write([]byte{0, 0})
	req.WriteString(authName)
	req.Write(make([]byte, pad4(len(authName))))
	req.Write(authData)
	req.Write(make([]byte, pad4(len(authData))))
	if _, err := c.Write(req.Bytes()); err != nil {
		return nil, err
	}
	var head [8]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return nil, fmt.Errorf("reading the setup reply: %w", err)
	}
	extra := int(binary.LittleEndian.Uint16(head[6:])) * 4
	if extra > MaxSetupReply {
		return nil, errors.New("setup reply too long")
	}
	body := make([]byte, extra)
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, fmt.Errorf("reading the setup reply: %w", err)
	}
	switch head[0] {
	case 0: // Failed: head[1] is the length of the reason
		n := int(head[1])
		if n > len(body) {
			n = len(body)
		}
		return nil, &RefusedError{Reason: printable(body[:n])}
	case 2: // Authenticate
		return nil, &RefusedError{Reason: printable(bytes.TrimRight(body, "\x00"))}
	case 1:
	default:
		return nil, fmt.Errorf("unknown setup reply status %d", head[0])
	}
	if len(body) < 32 {
		return nil, errors.New("setup reply too short")
	}
	s := &Setup{Release: binary.LittleEndian.Uint32(body[0:])}
	vlen := int(binary.LittleEndian.Uint16(body[16:]))
	screens, formats := int(body[20]), int(body[21])
	off := 32
	if off+vlen > len(body) {
		return nil, errors.New("setup reply too short")
	}
	s.Vendor = printable(body[off : off+vlen])
	off += vlen + pad4(vlen) + 8*formats
	if screens > 0 && off+24 <= len(body) {
		s.Width = binary.LittleEndian.Uint16(body[off+20:])
		s.Height = binary.LittleEndian.Uint16(body[off+22:])
	}
	return s, nil
}

func printable(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			out = append(out, c)
		} else {
			out = append(out, '?')
		}
	}
	return string(out)
}

// VNC authentication: DES with the password (at most 8 bytes, zero-padded)
// as key, each key byte bit-reversed, as the original VNC implementation
// did. The password file obfuscates the password the same way with a fixed
// key.

var vncFixedKey = []byte{23, 82, 107, 6, 35, 78, 88, 7}

// MaxVNCPassword is the number of password characters VNC uses.
const MaxVNCPassword = 8

func reverseBits(k []byte) []byte {
	out := make([]byte, len(k))
	for i, b := range k {
		var r byte
		for j := 0; j < 8; j++ {
			if b&(1<<j) != 0 {
				r |= 0x80 >> j
			}
		}
		out[i] = r
	}
	return out
}

func desEncrypt(key, block []byte) []byte {
	c, err := des.NewCipher(reverseBits(key))
	if err != nil {
		panic(err) // the key always has 8 bytes
	}
	out := make([]byte, len(block))
	for i := 0; i+8 <= len(block); i += 8 {
		c.Encrypt(out[i:i+8], block[i:i+8])
	}
	return out
}

func vncKey(password string) ([]byte, error) {
	if len(password) == 0 || len(password) > MaxVNCPassword {
		return nil, fmt.Errorf("a VNC password has 1 to %d characters", MaxVNCPassword)
	}
	k := make([]byte, 8)
	copy(k, password)
	return k, nil
}

// VNCPasswordFile is the content of a TigerVNC password file (what
// `vncpasswd -f` writes) for password.
func VNCPasswordFile(password string) ([]byte, error) {
	k, err := vncKey(password)
	if err != nil {
		return nil, err
	}
	return desEncrypt(vncFixedKey, k), nil
}

// VNCResponse answers the 16-byte challenge of VNC authentication.
func VNCResponse(challenge []byte, password string) ([]byte, error) {
	if len(challenge) != 16 {
		return nil, errors.New("a VNC challenge has 16 bytes")
	}
	k, err := vncKey(password)
	if err != nil {
		return nil, err
	}
	return desEncrypt(k, challenge), nil
}
