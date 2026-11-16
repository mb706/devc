package gui

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"time"

	"github.com/mb706/devc/internal/display"
)

// passwordAlphabet: letters and digits, so the password needs no escaping
// in a URL and can be typed into a VNC client.
const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Secrets are generated for every start of a sidecar.
type Secrets struct {
	// Cookie is the X authorisation cookie (MIT-MAGIC-COOKIE-1).
	Cookie []byte
	// Password is the VNC password: eight characters, all VNC uses
	// (about 47 bits), which is why the VNC port listens on the box's
	// loopback interface only.
	Password string
}

// NewSecrets draws a cookie and a password from rnd (crypto/rand when nil).
func NewSecrets(rnd io.Reader) (*Secrets, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	cookie := make([]byte, display.CookieLen)
	if _, err := io.ReadFull(rnd, cookie); err != nil {
		return nil, err
	}
	pw, err := newPassword(rnd, display.MaxVNCPassword)
	if err != nil {
		return nil, err
	}
	return &Secrets{Cookie: cookie, Password: pw}, nil
}

// newPassword draws n characters uniformly from passwordAlphabet by
// rejection sampling.
func newPassword(rnd io.Reader, n int) (string, error) {
	const limit = 256 - 256%len(passwordAlphabet)
	out := make([]byte, 0, n)
	buf := make([]byte, 1)
	for tries := 0; len(out) < n; tries++ {
		if tries > 64*n {
			return "", errors.New("the random source repeats itself")
		}
		if _, err := io.ReadFull(rnd, buf); err != nil {
			return "", err
		}
		if int(buf[0]) < limit {
			out = append(out, passwordAlphabet[int(buf[0])%len(passwordAlphabet)])
		}
	}
	return string(out), nil
}

// XAuthority is the Xauthority file for the display (box and sidecar get
// the same one).
func (s *Secrets) XAuthority() ([]byte, error) {
	return display.CookieFile(s.Cookie, DisplayNumber)
}

// Archive is what `devc-desktop secrets` reads on its standard input: a
// tar archive with the files xauthority and vncpasswd (mode 0600).
func (s *Secrets) Archive() ([]byte, error) {
	xa, err := s.XAuthority()
	if err != nil {
		return nil, err
	}
	pw, err := display.VNCPasswordFile(s.Password)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range []struct {
		name string
		data []byte
	}{{"xauthority", xa}, {"vncpasswd", pw}} {
		h := &tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.data)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
