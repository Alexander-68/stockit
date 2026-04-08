package app

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureTLSCertificateGeneratesSelfSignedFiles(t *testing.T) {
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "cert.pem")
	keyPath := filepath.Join(tempDir, "key.pem")

	if err := ensureTLSCertificate(certPath, keyPath, []string{"localhost", "127.0.0.1"}, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("ensure tls certificate: %v", err)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("generated key file is empty")
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode generated certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	if certificate.Subject.CommonName != "StockIt Self-Signed Certificate" {
		t.Fatalf("unexpected common name: %q", certificate.Subject.CommonName)
	}
	if !containsString(certificate.DNSNames, "localhost") {
		t.Fatalf("localhost missing from dns names: %v", certificate.DNSNames)
	}
	if !containsIP(certificate.IPAddresses, net.ParseIP("127.0.0.1")) {
		t.Fatalf("127.0.0.1 missing from ip addresses: %v", certificate.IPAddresses)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsIP(values []net.IP, want net.IP) bool {
	for _, value := range values {
		if value.Equal(want) {
			return true
		}
	}
	return false
}

func TestEnsureTLSCertificateRotatesWhenExpiring(t *testing.T) {
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "cert.pem")
	keyPath := filepath.Join(tempDir, "key.pem")

	// Generate a fresh certificate dated one year before "now1".
	now1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := ensureTLSCertificate(certPath, keyPath, []string{"localhost"}, now1); err != nil {
		t.Fatalf("first ensure: %v", err)
	}

	originalCertBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read original cert: %v", err)
	}
	originalCert := parseCertFile(t, certPath)
	originalNotAfter := originalCert.NotAfter

	// Calling again with the same now should be a no-op (cert is fresh).
	if err := ensureTLSCertificate(certPath, keyPath, []string{"localhost"}, now1); err != nil {
		t.Fatalf("second ensure (no-op): %v", err)
	}
	currentBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("re-read cert: %v", err)
	}
	if string(currentBytes) != string(originalCertBytes) {
		t.Fatal("certificate was unexpectedly regenerated when still fresh")
	}

	// Advance "now" to within the renewal window — must regenerate.
	nearExpiry := originalNotAfter.Add(-15 * 24 * time.Hour)
	if err := ensureTLSCertificate(certPath, keyPath, []string{"localhost"}, nearExpiry); err != nil {
		t.Fatalf("renewal ensure: %v", err)
	}
	renewedCert := parseCertFile(t, certPath)
	if !renewedCert.NotAfter.After(originalNotAfter) {
		t.Fatalf("renewed cert NotAfter %v should be after original %v", renewedCert.NotAfter, originalNotAfter)
	}
}

func TestExistingCertificateUsableHandlesMissingAndCorrupt(t *testing.T) {
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "cert.pem")
	keyPath := filepath.Join(tempDir, "key.pem")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if existingCertificateUsable(certPath, keyPath, now) {
		t.Fatal("expected missing files to be unusable")
	}

	if err := os.WriteFile(certPath, []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("write garbage cert: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write garbage key: %v", err)
	}
	if existingCertificateUsable(certPath, keyPath, now) {
		t.Fatal("expected garbage cert to be unusable")
	}
}

func parseCertFile(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cert %s: %v", path, err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("decode cert %s: empty pem", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert %s: %v", path, err)
	}
	return cert
}

func TestListenHostExtraction(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{name: "empty", addr: "", want: ""},
		{name: "loopback ipv4", addr: "127.0.0.1:8443", want: "127.0.0.1"},
		{name: "loopback ipv6", addr: "[::1]:8443", want: "::1"},
		{name: "wildcard ipv4", addr: "0.0.0.0:8443", want: ""},
		{name: "wildcard ipv6", addr: "[::]:8443", want: ""},
		{name: "named host", addr: "stockit.local:8443", want: "stockit.local"},
		{name: "no port", addr: "stockit.local", want: "stockit.local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := listenHost(tc.addr); got != tc.want {
				t.Fatalf("listenHost(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

func TestNormalizedCertificateHostsDeduplicatesAndSorts(t *testing.T) {
	got := normalizedCertificateHosts([]string{
		"localhost",
		"127.0.0.1",
		"localhost",
		"0.0.0.0",
		"::",
		"  stockit.local  ",
		"stockit.local",
	})
	want := []string{"127.0.0.1", "localhost", "stockit.local"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTLSHostsAggregatesListeners(t *testing.T) {
	got := tlsHosts("127.0.0.1:8080", "stockit.local:8443")
	want := map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true, "stockit.local": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, host := range got {
		if !want[host] {
			t.Fatalf("unexpected host %q in %v", host, got)
		}
	}

	// Wildcard listeners must not appear in SAN.
	wildcardHosts := tlsHosts("0.0.0.0:8080", "[::]:8443")
	for _, host := range wildcardHosts {
		if host == "0.0.0.0" || host == "::" {
			t.Fatalf("wildcard host %q leaked into SAN list: %v", host, wildcardHosts)
		}
	}
}
