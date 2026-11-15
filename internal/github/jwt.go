package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// JWT lifetimes: GitHub accepts at most ten minutes; iat lies a minute in
// the past against clock drift, as GitHub recommends.
const (
	jwtBackdate = time.Minute
	jwtLifetime = 9 * time.Minute
)

// ParsePrivateKey reads an App's private key in PEM form: PKCS #1 ("RSA
// PRIVATE KEY", what GitHub hands out) or PKCS #8 ("PRIVATE KEY").
func ParsePrivateKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("the App key is not in PEM form")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("the App key is not a valid RSA private key")
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("the App key is not a valid private key")
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("the App key is not an RSA key")
		}
		return rk, nil
	}
	return nil, fmt.Errorf("the App key is a PEM block of type %q, not an RSA private key", block.Type)
}

// AppJWT is the token an App authenticates itself with: an RS256 JWT with
// the App's ID as issuer, valid for nine minutes.
func AppJWT(key *rsa.PrivateKey, appID int64, now time.Time) (string, error) {
	if appID <= 0 {
		return "", errors.New("invalid App ID")
	}
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		IssuedAt  int64 `json:"iat"`
		ExpiresAt int64 `json:"exp"`
		Issuer    int64 `json:"iss"`
	}{now.Add(-jwtBackdate).Unix(), now.Add(jwtLifetime).Unix(), appID})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing the App token: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}
