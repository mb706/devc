// This file implements the encrypted secrets bundle that devc secret push
// stores on a hub and devc secret pull opens on another machine, so the
// hub keeps a realm's secrets without being able to read them.

package archive

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/mb706/devc/internal/proto"
)

// Sealed is an encrypted bundle of a realm's secrets (work order C17
// section 4): the key is derived from a passphrase with argon2id, the
// data sealed with XChaCha20-Poly1305 and bound to the realm's name. The
// hub stores it as it is and can read nothing but its shape.
type Sealed struct {
	Format string `json:"format"`
	V      int    `json:"v"`
	KDF    KDF    `json:"kdf"`
	Nonce  string `json:"nonce"`
	Data   string `json:"data"`
}

// KDF are the argon2id parameters and salt (base64).
type KDF struct {
	Alg     string `json:"alg"`
	Time    uint32 `json:"t"`
	Memory  uint32 `json:"m"` // KiB
	Threads uint8  `json:"p"`
	Salt    string `json:"salt"`
}

// SealedFormat names the format.
const SealedFormat = "devc-secrets"

// Limits of a sealed bundle.
const (
	// MaxSealed bounds the encoded ciphertext (the whole request stays
	// below the hub's general body limit of 1 MiB).
	MaxSealed = 512 << 10
	// The KDF parameters a bundle may ask for when it is opened: a hostile
	// hub must not be able to make the machine spend unbounded memory.
	maxKDFTime   = 10
	minKDFMemory = 8 << 10
	maxKDFMemory = 1 << 20
	maxKDFThread = 16
)

// The parameters Seal uses (RFC 9106, second recommended option).
const (
	sealTime    = 3
	sealMemory  = 64 << 10
	sealThreads = 4
)

// ErrOpen is the error for a wrong passphrase or damaged data; the two
// cannot be told apart.
var ErrOpen = errors.New("the passphrase is wrong or the data is damaged")

var b64 = base64.StdEncoding

func aad(realm string) []byte { return []byte("devc-secrets/v1\x00realm=" + realm) }

func deriveKey(passphrase []byte, k KDF) ([]byte, error) {
	salt, err := b64.DecodeString(k.Salt)
	if err != nil {
		return nil, err
	}
	return argon2.IDKey(passphrase, salt, k.Time, k.Memory, k.Threads, chacha20poly1305.KeySize), nil
}

// Seal encrypts plain for realm with a key derived from passphrase.
func Seal(passphrase []byte, realm string, plain []byte) (*Sealed, error) {
	if len(passphrase) == 0 {
		return nil, errors.New("the passphrase is empty")
	}
	salt := make([]byte, 16)
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	s := &Sealed{Format: SealedFormat, V: 1, KDF: KDF{Alg: "argon2id", Time: sealTime, Memory: sealMemory, Threads: sealThreads, Salt: b64.EncodeToString(salt)},
		Nonce: b64.EncodeToString(nonce)}
	key, err := deriveKey(passphrase, s.KDF)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	s.Data = b64.EncodeToString(aead.Seal(nil, nonce, plain, aad(realm)))
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Validate checks the shape of a bundle: format, parameters within bounds,
// well-formed base64 of the right lengths, the size limit.
func (s *Sealed) Validate() error {
	switch {
	case s.Format != SealedFormat || s.V != 1:
		return errors.New("not a devc secrets bundle of version 1")
	case s.KDF.Alg != "argon2id":
		return fmt.Errorf("unknown key derivation %q", s.KDF.Alg)
	case s.KDF.Time < 1 || s.KDF.Time > maxKDFTime || s.KDF.Memory < minKDFMemory || s.KDF.Memory > maxKDFMemory ||
		s.KDF.Threads < 1 || s.KDF.Threads > maxKDFThread:
		return errors.New("key derivation parameters out of bounds")
	case len(s.Data) > MaxSealed:
		return errors.New("the bundle is too large")
	}
	salt, err := b64.DecodeString(s.KDF.Salt)
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return errors.New("invalid salt")
	}
	nonce, err := b64.DecodeString(s.Nonce)
	if err != nil || len(nonce) != chacha20poly1305.NonceSizeX {
		return errors.New("invalid nonce")
	}
	data, err := b64.DecodeString(s.Data)
	if err != nil || len(data) < chacha20poly1305.Overhead {
		return errors.New("invalid ciphertext")
	}
	return nil
}

// Open decrypts a bundle for realm. A wrong passphrase, another realm and
// damaged data all give ErrOpen.
func (s *Sealed) Open(passphrase []byte, realm string) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	key, err := deriveKey(passphrase, s.KDF)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce, _ := b64.DecodeString(s.Nonce)
	data, _ := b64.DecodeString(s.Data)
	plain, err := aead.Open(nil, nonce, data, aad(realm))
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

// DecodeSealed decodes a bundle strictly (no unknown fields, nothing after
// it) and validates its shape.
func DecodeSealed(b []byte) (*Sealed, error) {
	if len(b) > MaxSealed+4096 {
		return nil, errors.New("the bundle is too large")
	}
	var s Sealed
	if err := proto.StrictJSON(b, &s); err != nil {
		return nil, fmt.Errorf("invalid secrets bundle: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}
