// Enrolment (work order C11b section 2). `devc hub enroll machine|device
// <name>` creates a single-use token valid for at most 15 minutes. While
// one is outstanding, /enroll/<token> is reachable without a client
// certificate (Server.gate):
//
//   - GET by a device's browser downloads the PKCS#12 bundle made when the
//     token was created; its passphrase was shown only in the terminal that
//     ran enroll. The certificate is registered at the download.
//   - POST by `devc join --token` carries a certificate signing request:
//     the key was made on the machine and never leaves it. The hub signs a
//     machine certificate for the name the token was made for; only the
//     public key comes from the request.
//
// The token is the credential: unknown, used and expired tokens get 404,
// and count as failures for the rate limit. The server reads the CA key
// only to sign such a request.

package hub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// maxEnrolBody bounds an enrolment request (a CSR).
const maxEnrolBody = 16 << 10

// NewEnrolment is a token made by CreateEnrolment.
type NewEnrolment struct {
	Token   string
	Role    string
	Name    string
	Expires time.Time
	// Passphrase protects a device's bundle; it is shown once.
	Passphrase string
}

// CreateEnrolment makes a single-use enrolment token for a machine or
// device name, valid for ttl (at most EnrolTTL). For a device, the key,
// certificate and PKCS#12 bundle are made now and kept with the token
// until it is used or expires.
func CreateEnrolment(l Layout, st *Store, role, name string, ttl time.Duration, legacy bool) (*NewEnrolment, error) {
	if role != RoleMachine && role != RoleDevice {
		return nil, fmt.Errorf("unknown role %q", role)
	}
	if ttl <= 0 || ttl > EnrolTTL {
		return nil, fmt.Errorf("an enrolment token is valid for at most %s", EnrolTTL)
	}
	token, id, err := NewToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	e := Enrolment{ID: id, Role: role, Name: name, Created: now, Expires: now.Add(ttl)}
	out := &NewEnrolment{Token: token, Role: role, Name: name, Expires: e.Expires}
	if role == RoleDevice {
		ca, err := LoadCA(l)
		if err != nil {
			return nil, err
		}
		is, err := ca.IssueClient(RoleDevice, name, legacy)
		if err != nil {
			return nil, err
		}
		e.Bundle, e.CertPEM, out.Passphrase = is.P12, string(is.CertPEM), is.Passphrase
	}
	if err := st.AddEnrolment(e); err != nil {
		return nil, err
	}
	return out, nil
}

// enrolToken reads the token of an enrolment path; on failure it has
// answered 404.
func (s *Server) enrolToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	tok := r.PathValue("token")
	if !ValidToken(tok) {
		s.enrolRefused(w, r, "malformed token")
		return "", false
	}
	return tok, true
}

func (s *Server) enrolRefused(w http.ResponseWriter, r *http.Request, why string) {
	s.lim.fail("a:"+s.clientAddr(r), s.now())
	s.log.Info("enrolment refused", "addr", s.clientAddr(r), "why", why)
	w.Header().Set("Connection", "close")
	writeError(w, http.StatusNotFound, "not found")
}

// enrolDevice serves a device's bundle.
func (s *Server) enrolDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		// A HEAD (a link preview, a prefetch) must not use the token up.
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tok, ok := s.enrolToken(w, r)
	if !ok {
		return
	}
	var rec CertRecord
	e, err := s.st.UseEnrolment(TokenID(tok), RoleDevice, s.now(), func(e Enrolment, tx *sql.Tx) error {
		cert, err := ParseCertPEM([]byte(e.CertPEM))
		if err != nil || len(e.Bundle) == 0 {
			return errors.New("the enrolment holds no bundle")
		}
		rec = CertRecord{Fingerprint: Fingerprint(cert), Role: RoleDevice, Name: e.Name, Serial: cert.SerialNumber.Text(16), Issued: cert.NotBefore, Expires: cert.NotAfter}
		return addCertTx(tx, rec)
	})
	if errors.Is(err, errTokenUnusable) {
		s.enrolRefused(w, r, "unknown, used or expired device token")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.log.Info("device enrolled", "device", e.Name, "fingerprint", rec.Fingerprint[:16], "addr", s.clientAddr(r))
	w.Header().Set("Content-Type", "application/x-pkcs12")
	w.Header().Set("Content-Disposition", `attachment; filename="`+e.Name+`.p12"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.Bundle)
}

// EnrolMachineRequest is the body of POST /enroll/<token>.
type EnrolMachineRequest struct {
	CSR string `json:"csr"`
}

// EnrolMachineReply is the answer: the machine's name and certificate.
type EnrolMachineReply struct {
	Name        string `json:"name"`
	Certificate string `json:"certificate"`
}

// enrolMachine signs a machine's certificate request.
func (s *Server) enrolMachine(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.enrolToken(w, r)
	if !ok {
		return
	}
	// Nothing of the request is looked at before the token.
	if ok, err := s.st.EnrolmentUsable(TokenID(tok), RoleMachine, s.now()); err != nil {
		s.internal(w, err)
		return
	} else if !ok {
		s.enrolRefused(w, r, "unknown, used or expired machine token")
		return
	}
	var req EnrolMachineRequest
	if !decodeBody(w, r, &req) {
		return
	}
	csr, err := ParseCSR([]byte(req.CSR))
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	var issued []byte
	e, err := s.st.UseEnrolment(TokenID(tok), RoleMachine, s.now(), func(e Enrolment, tx *sql.Tx) error {
		ca, err := LoadCA(s.l)
		if err != nil {
			return err
		}
		cert, err := ca.signClient(RoleMachine, e.Name, csr.PublicKey)
		if err != nil {
			return err
		}
		issued = certPEM(cert.Raw)
		return addCertTx(tx, CertRecord{Fingerprint: Fingerprint(cert), Role: RoleMachine, Name: e.Name, Serial: cert.SerialNumber.Text(16), Issued: cert.NotBefore, Expires: cert.NotAfter})
	})
	if errors.Is(err, errTokenUnusable) {
		s.enrolRefused(w, r, "unknown, used or expired machine token")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.log.Info("machine enrolled", "machine", e.Name, "addr", s.clientAddr(r))
	writeJSON(w, http.StatusOK, EnrolMachineReply{Name: e.Name, Certificate: string(issued)})
}

// ParseCSR parses and checks a PEM certificate request: its signature
// (the requester holds the key) and an ECDSA P-256 key.
func ParseCSR(b []byte) (*x509.CertificateRequest, error) {
	blk, rest := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("the request must hold one PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid certificate request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, errors.New("the certificate request's signature does not verify")
	}
	k, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, errors.New("the certificate request must carry an ECDSA P-256 key")
	}
	return csr, nil
}

// NewMachineCSR makes a key and a certificate request for enrolment (on
// the machine). The hub decides the name.
func NewMachineCSR() (keyOut, csrOut []byte, err error) {
	key, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{Organization: []string{Organisation}}}, key)
	if err != nil {
		return nil, nil, err
	}
	kp, err := keyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return kp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}
