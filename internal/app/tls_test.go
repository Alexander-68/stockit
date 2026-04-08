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
