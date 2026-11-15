package hub

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

func TestInitAndIssue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hub")
	if err := Init(dir, "hub.example.org:8443"); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir, ""); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("second init: %v", err)
	}
	l := Layout{Dir: dir}
	for f, mode := range map[string]os.FileMode{dir: 0o700, l.CAKey(): 0o600, l.ServerKey(): 0o600, l.DB(): 0o600, l.CACert(): 0o644} {
		fi, err := os.Stat(f)
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v, want %o", f, fi.Mode(), err, mode)
		}
	}
	ca, err := LoadCA(l)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Cert.IsCA || ca.Cert.NotAfter.Sub(time.Now()) < 9*365*24*time.Hour {
		t.Errorf("CA: %v until %s", ca.Cert.IsCA, ca.Cert.NotAfter)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)

	// The server certificate is for the public name's host.
	b, _ := os.ReadFile(l.ServerCert())
	srv, err := ParseCertPEM(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Verify(x509.VerifyOptions{Roots: pool, DNSName: "hub.example.org"}); err != nil {
		t.Errorf("server certificate: %v", err)
	}
	if _, err := os.Stat(l.ServerCSR()); err != nil {
		t.Error(err)
	}

	for _, role := range []string{RoleMachine, RoleDevice} {
		is, err := ca.IssueClient(role, "n1", false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := is.Cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			t.Errorf("%s: %v", role, err)
		}
		if RoleOf(is.Cert) != role || is.Cert.Subject.CommonName != "n1" || is.Fingerprint != Fingerprint(is.Cert) {
			t.Errorf("%s: subject %v", role, is.Cert.Subject)
		}
		if d := is.Cert.NotAfter.Sub(time.Now()); d < 364*24*time.Hour || d > 366*24*time.Hour {
			t.Errorf("%s: lifetime %s", role, d)
		}
		// A client certificate is no server certificate.
		if _, err := is.Cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
			t.Errorf("%s certificate verifies as a server certificate", role)
		}
		if role == RoleMachine && (is.P12 != nil || is.Passphrase != "") {
			t.Error("a machine got a PKCS#12 bundle")
		}
		if role == RoleDevice {
			if len(is.Passphrase) != 23 {
				t.Errorf("passphrase %q", is.Passphrase)
			}
			key, cert, chain, err := pkcs12.DecodeChain(is.P12, is.Passphrase)
			if err != nil || key == nil || cert.Subject.CommonName != "n1" || len(chain) != 1 || !chain[0].Equal(ca.Cert) {
				t.Errorf("bundle: %v %v", cert, err)
			}
			if _, _, _, err := pkcs12.DecodeChain(is.P12, "wrong"); err == nil {
				t.Error("the bundle opens with a wrong passphrase")
			}
		}
	}
	legacy, err := ca.IssueClient(RoleDevice, "old", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := pkcs12.DecodeChain(legacy.P12, legacy.Passphrase); err != nil {
		t.Errorf("legacy bundle: %v", err)
	}
	if _, err := ca.IssueClient("admin", "x", false); err == nil {
		t.Error("an unknown role was issued")
	}
	if err := ca.IssueServer(l, "bad host"); err == nil {
		t.Error("an invalid server name was accepted")
	}
}

func TestRegistry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hub")
	if err := Init(dir, ""); err != nil {
		t.Fatal(err)
	}
	l := Layout{Dir: dir}
	ca, _ := LoadCA(l)
	st, err := OpenStore(l.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	add := func(role, name string) *Issued {
		is, err := ca.IssueClient(role, name, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AddCert(CertRecord{Fingerprint: is.Fingerprint, Role: role, Name: name, Serial: "1", Issued: is.Cert.NotBefore, Expires: is.Cert.NotAfter}); err != nil {
			t.Fatal(err)
		}
		return is
	}
	g0, _ := st.RegistryGen()
	m1 := add(RoleMachine, "m1")
	d1 := add(RoleDevice, "m1")
	d2 := add(RoleDevice, "phone")
	g1, _ := st.RegistryGen()
	if g0 == g1 {
		t.Error("issuing did not change the registry generation")
	}
	rec, ok, err := st.Cert(m1.Fingerprint)
	if err != nil || !ok || !rec.Valid(time.Now()) || rec.Role != RoleMachine {
		t.Errorf("machine record: %+v %v %v", rec, ok, err)
	}
	if rec.Valid(rec.Expires.Add(time.Second)) {
		t.Error("an expired certificate is valid")
	}
	if _, ok, _ := st.Cert(strings.Repeat("0", 64)); ok {
		t.Error("an unknown fingerprint was found")
	}
	// Revoke by name and role, then by fingerprint prefix.
	hit, err := st.Revoke("m1", RoleDevice)
	if err != nil || len(hit) != 1 || hit[0].Fingerprint != d1.Fingerprint {
		t.Fatalf("revoke m1 device: %+v %v", hit, err)
	}
	if rec, _, _ := st.Cert(d1.Fingerprint); rec.Valid(time.Now()) || rec.Revoked == nil {
		t.Error("the revoked certificate is valid")
	}
	if rec, _, _ := st.Cert(m1.Fingerprint); !rec.Valid(time.Now()) {
		t.Error("the machine certificate of the same name was revoked")
	}
	if g2, _ := st.RegistryGen(); g2 == g1 {
		t.Error("revoking did not change the registry generation")
	}
	if hit, _ := st.Revoke(d2.Fingerprint[:12], ""); len(hit) != 1 {
		t.Errorf("revoke by fingerprint: %+v", hit)
	}
	if hit, _ := st.Revoke(d2.Fingerprint[:6], ""); len(hit) != 0 {
		t.Error("a short prefix matched")
	}
	if hit, _ := st.Revoke("phone", ""); len(hit) != 0 {
		t.Error("a revoked certificate was revoked again")
	}
	all, _ := st.Certs()
	if len(all) != 3 {
		t.Errorf("certs: %d", len(all))
	}
}
