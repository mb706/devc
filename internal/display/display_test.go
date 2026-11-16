package display

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

// The vectors are what TigerVNC's `vncpasswd -f` (Debian bookworm) wrote.
func TestVNCPasswordFile(t *testing.T) {
	for pw, want := range map[string]string{
		"password": "dbd83cfd727a1458",
		"Secure!!": "1b8167bc0099c7dc",
		"abc":      "9c0a172d3482e122",
	} {
		got, err := VNCPasswordFile(pw)
		if err != nil || hex.EncodeToString(got) != want {
			t.Errorf("VNCPasswordFile(%q) = %x, %v; want %s", pw, got, err, want)
		}
	}
	for _, bad := range []string{"", "123456789"} {
		if _, err := VNCPasswordFile(bad); err == nil {
			t.Errorf("VNCPasswordFile(%q) succeeded", bad)
		}
	}
}

// TestVNCResponse: the response is DES of the challenge with the password's
// bit-reversed bytes as key; decrypting with the same key gives the
// challenge back, and another password gives another response.
func TestVNCResponse(t *testing.T) {
	ch := []byte("0123456789abcdef")
	r1, err := VNCResponse(ch, "secret12")
	if err != nil || len(r1) != 16 {
		t.Fatal(r1, err)
	}
	r2, _ := VNCResponse(ch, "secret13")
	if bytes.Equal(r1, r2) || bytes.Equal(r1, ch) {
		t.Error("responses do not depend on the password")
	}
	// The password file is the response of the fixed key to the password.
	pf, _ := VNCPasswordFile("password")
	k := string(vncFixedKey)
	resp, _ := VNCResponse([]byte("password"+"password"), k)
	if !bytes.Equal(resp[:8], pf) || !bytes.Equal(resp[8:], pf) {
		t.Errorf("file %x, response %x", pf, resp)
	}
	if _, err := VNCResponse(ch[:8], "x"); err == nil {
		t.Error("short challenge accepted")
	}
}

func TestXAuthority(t *testing.T) {
	cookie := bytes.Repeat([]byte{0xab}, CookieLen)
	b, err := CookieFile(cookie, 1)
	if err != nil {
		t.Fatal(err)
	}
	// family ffff, empty address, number "1", the protocol name, the cookie.
	want := "ffff" + "0000" + "0001" + "31" + "0012" + hex.EncodeToString([]byte(CookieName)) + "0010" + strings.Repeat("ab", 16)
	if hex.EncodeToString(b) != want {
		t.Errorf("cookie file %x", b)
	}
	es, err := ParseXAuthority(b)
	if err != nil || len(es) != 1 || es[0].Family != FamilyWild || es[0].Number != "1" || !bytes.Equal(es[0].Data, cookie) {
		t.Fatalf("parse: %+v %v", es, err)
	}
	for _, addr := range [][]byte{nil, {127, 0, 0, 1}} {
		if got, ok := FindCookie(es, 1, addr); !ok || !bytes.Equal(got, cookie) {
			t.Errorf("FindCookie(%v) = %x %v", addr, got, ok)
		}
	}
	if _, ok := FindCookie(es, 2, nil); ok {
		t.Error("cookie for display 2")
	}
	// Specific families match only their connections.
	other, _ := EncodeXAuthority([]XAuthEntry{
		{Family: FamilyInternet, Address: []byte{10, 0, 0, 1}, Number: "1", Name: CookieName, Data: []byte("inet10")},
		{Family: FamilyLocal, Address: []byte("host"), Number: "1", Name: CookieName, Data: []byte("local")},
		{Family: FamilyInternet, Address: []byte{127, 0, 0, 1}, Number: "1", Name: "XDM-AUTHORIZATION-1", Data: []byte("xdm")},
		{Family: FamilyInternet, Address: []byte{127, 0, 0, 1}, Number: "1", Name: CookieName, Data: []byte("inet127")},
	})
	es, _ = ParseXAuthority(other)
	if got, _ := FindCookie(es, 1, []byte{127, 0, 0, 1}); string(got) != "inet127" {
		t.Errorf("tcp: %q", got)
	}
	if got, _ := FindCookie(es, 1, nil); string(got) != "local" {
		t.Errorf("local: %q", got)
	}
	for _, bad := range [][]byte{b[:1], b[:5], b[:len(b)-1], make([]byte, MaxXAuthority+1)} {
		if _, err := ParseXAuthority(bad); err == nil {
			t.Errorf("parsed %d bytes", len(bad))
		}
	}
	if _, err := CookieFile([]byte("short"), 1); err == nil {
		t.Error("short cookie accepted")
	}
}

// fakeServer answers one X connection setup on c: it checks the request,
// then sends reply.
func fakeServer(t *testing.T, c net.Conn, wantName string, wantData []byte, reply []byte) {
	defer c.Close()
	var head [12]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		t.Errorf("server: %v", err)
		return
	}
	if head[0] != 'l' || binary.LittleEndian.Uint16(head[2:]) != 11 {
		t.Errorf("setup request head %x", head)
	}
	n, d := int(binary.LittleEndian.Uint16(head[6:])), int(binary.LittleEndian.Uint16(head[8:]))
	rest := make([]byte, n+pad4(n)+d+pad4(d))
	if _, err := io.ReadFull(c, rest); err != nil {
		t.Errorf("server: %v", err)
		return
	}
	if string(rest[:n]) != wantName || !bytes.Equal(rest[n+pad4(n):n+pad4(n)+d], wantData) {
		t.Errorf("auth %q %x", rest[:n], rest[n+pad4(n):n+pad4(n)+d])
	}
	_, _ = c.Write(reply)
}

func successReply(vendor string, w, h uint16) []byte {
	var body bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&body, le, uint32(12101006)) // release
	_ = binary.Write(&body, le, [3]uint32{0x400000, 0x1fffff, 256})
	_ = binary.Write(&body, le, uint16(len(vendor)))
	_ = binary.Write(&body, le, uint16(65535))
	body.Write([]byte{1, 1, 0, 0, 32, 32, 8, 255, 0, 0, 0, 0}) // 1 screen, 1 format
	body.WriteString(vendor)
	body.Write(make([]byte, pad4(len(vendor))))
	body.Write([]byte{24, 32, 32, 0, 0, 0, 0, 0}) // one format
	screen := make([]byte, 40)
	le.PutUint16(screen[20:], w)
	le.PutUint16(screen[22:], h)
	body.Write(screen)
	head := []byte{1, 0, 11, 0, 0, 0, 0, 0}
	le.PutUint16(head[6:], uint16(body.Len()/4))
	return append(head, body.Bytes()...)
}

func TestHandshake(t *testing.T) {
	cookie := bytes.Repeat([]byte{7}, 16)
	run := func(name string, data []byte, reply []byte) (*Setup, error) {
		a, b := net.Pipe()
		go fakeServer(t, b, name, data, reply)
		defer a.Close()
		return Handshake(a, name, data)
	}
	s, err := run(CookieName, cookie, successReply("The X.Org Foundation", 1600, 900))
	if err != nil || s.Vendor != "The X.Org Foundation" || s.Release != 12101006 || s.Width != 1600 || s.Height != 900 {
		t.Errorf("success: %+v %v", s, err)
	}
	reason := "Authorization required, but no authorization protocol specified\n"
	failed := append([]byte{0, byte(len(reason)), 11, 0, 0, 0, 0, 0}, []byte(reason)...)
	failed = append(failed, make([]byte, pad4(len(reason)))...)
	binary.LittleEndian.PutUint16(failed[6:], uint16((len(reason)+pad4(len(reason)))/4))
	_, err = run("", nil, failed)
	var re *RefusedError
	if !errors.As(err, &re) || !strings.HasPrefix(re.Reason, "Authorization required") {
		t.Errorf("failed: %v", err)
	}
	auth := []byte{2, 0, 0, 0, 0, 0, 1, 0, 'n', 'o', 0, 0}
	if _, err := run(CookieName, cookie, auth); !errors.As(err, &re) || re.Reason != "no" {
		t.Errorf("authenticate: %v", err)
	}
	if _, err := run("", nil, []byte{1, 0, 11}); err == nil || errors.As(err, &re) {
		t.Errorf("truncated: %v", err)
	}
	if _, err := run("", nil, []byte{9, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Error("unknown status accepted")
	}
}
