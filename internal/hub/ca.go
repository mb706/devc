package hub

// The hub's private CA (work order C11a section 1): an ECDSA P-256 CA valid
// for ten years signs one-year client certificates for machines and
// devices, with the name as common name and the role as organisational
// unit. Issued certificates are recorded in the registry by fingerprint;
// the registry, not the certificate, is what the server believes.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

// Roles of client certificates.
const (
	RoleMachine = "machine"
	RoleDevice  = "device"
)

// Lifetimes.
const (
	caLifetime     = 10 * 365 * 24 * time.Hour
	clientLifetime = 365 * 24 * time.Hour
	serverLifetime = 365 * 24 * time.Hour
	// backdate tolerates clocks that are a little behind.
	backdate = 5 * time.Minute
)

// Organisation is the O of every certificate the CA issues.
const Organisation = "devc"

// Fingerprint is the hex SHA-256 of a certificate's DER encoding.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func newKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func keyPEM(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// writeNew writes a new file with mode, refusing to replace anything.
func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s exists already: remove it or choose another directory", path)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// CA is the loaded CA.
type CA struct {
	Cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// createCA writes a new CA into dir/ca (ca.crt, ca.key 0600).
func createCA(l Layout) error {
	key, err := newKey()
	if err != nil {
		return err
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "devc hub CA", Organization: []string{Organisation}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kp, err := keyPEM(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.CAKey()), 0o700); err != nil {
		return err
	}
	if err := writeNew(l.CAKey(), kp, 0o600); err != nil {
		return err
	}
	return writeNew(l.CACert(), certPEM(der), 0o644)
}

// LoadCACert reads the CA certificate (all the server needs).
func LoadCACert(l Layout) (*x509.Certificate, error) {
	b, err := os.ReadFile(l.CACert())
	if err != nil {
		return nil, fmt.Errorf("reading the hub CA: %w (run 'devc hub init')", err)
	}
	return ParseCertPEM(b)
}

// LoadCA reads the CA certificate and key (for issuing).
func LoadCA(l Layout) (*CA, error) {
	cert, err := LoadCACert(l)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(l.CAKey())
	if err != nil {
		return nil, fmt.Errorf("reading the hub CA key: %w", err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("the hub CA key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the hub CA key: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("the hub CA key is not an ECDSA key")
	}
	return &CA{Cert: cert, key: key}, nil
}

// ParseCertPEM parses the first certificate of a PEM file.
func ParseCertPEM(b []byte) (*x509.Certificate, error) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, errors.New("no PEM certificate found")
		}
		if blk.Type == "CERTIFICATE" {
			return x509.ParseCertificate(blk.Bytes)
		}
	}
}

// Issued is a newly issued client certificate.
type Issued struct {
	Role, Name  string
	Cert        *x509.Certificate
	CertPEM     []byte
	KeyPEM      []byte
	Fingerprint string
	// P12 and Passphrase are set for devices.
	P12        []byte
	Passphrase string
}

// IssueClient creates a key and a one-year client certificate for name
// with role in the subject. Devices also get a PKCS#12 bundle (key,
// certificate and the CA) protected by a generated passphrase; legacy
// selects the weaker encryption older phones need.
func (ca *CA) IssueClient(role, name string, legacy bool) (*Issued, error) {
	if role != RoleMachine && role != RoleDevice {
		return nil, fmt.Errorf("unknown role %q", role)
	}
	key, err := newKey()
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name, OrganizationalUnit: []string{role}, Organization: []string{Organisation}},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(clientLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	kp, err := keyPEM(key)
	if err != nil {
		return nil, err
	}
	is := &Issued{Role: role, Name: name, Cert: cert, CertPEM: certPEM(der), KeyPEM: kp, Fingerprint: Fingerprint(cert)}
	if role == RoleDevice {
		is.Passphrase, err = newPassphrase()
		if err != nil {
			return nil, err
		}
		enc := pkcs12.Modern
		if legacy {
			enc = pkcs12.Legacy
		}
		is.P12, err = enc.Encode(key, cert, []*x509.Certificate{ca.Cert}, is.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("encoding the PKCS#12 bundle: %w", err)
		}
	}
	return is, nil
}

// newPassphrase returns four groups of five characters from an alphabet
// without look-alikes (about 96 bits).
func newPassphrase() (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var s strings.Builder
	for i, v := range b {
		if i > 0 && i%5 == 0 {
			s.WriteByte('-')
		}
		s.WriteByte(alphabet[int(v)%len(alphabet)])
	}
	return s.String(), nil
}

// IssueServer creates the hub's own TLS key, a certificate request for a
// public CA, and a one-year server certificate from the devc CA, for host
// (a DNS name or an IP address), in the server directory. Machines that
// join with the devc CA (--ca) accept that certificate; browsers need a
// certificate from a public CA or the devc CA installed.
func (ca *CA) IssueServer(l Layout, host string) error {
	if net.ParseIP(host) == nil && !ValidHost(host) {
		return fmt.Errorf("%q is not a host name or IP address", host)
	}
	key, err := newKey()
	if err != nil {
		return err
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	subject := pkix.Name{CommonName: host, Organization: []string{Organisation}}
	var dns []string
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		dns = []string{host}
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      subject,
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(serverLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: subject, DNSNames: dns, IPAddresses: ips}, key)
	if err != nil {
		return err
	}
	kp, err := keyPEM(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.ServerKey()), 0o700); err != nil {
		return err
	}
	for _, f := range []string{l.ServerKey(), l.ServerCert(), l.ServerCSR()} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := writeNew(l.ServerKey(), kp, 0o600); err != nil {
		return err
	}
	if err := writeNew(l.ServerCSR(), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), 0o644); err != nil {
		return err
	}
	return writeNew(l.ServerCert(), append(certPEM(der), certPEM(ca.Cert.Raw)...), 0o644)
}

// RoleOf returns the role a certificate's subject names (for messages; the
// server takes the role from the registry).
func RoleOf(cert *x509.Certificate) string {
	if len(cert.Subject.OrganizationalUnit) == 1 {
		return cert.Subject.OrganizationalUnit[0]
	}
	return ""
}
