package app

import (
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const selfSignedCertificateValidity = 365 * 24 * time.Hour

func ensureTLSCertificate(certFile, keyFile string, hosts []string, now time.Time) error {
	certExists := fileExists(certFile)
	keyExists := fileExists(keyFile)
	if certExists && keyExists {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(certFile), 0o755); err != nil {
		return fmt.Errorf("create certificate dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o755); err != nil {
		return fmt.Errorf("create key dir: %w", err)
	}

	privateKey, err := rsa.GenerateKey(nil, 2048)
	if err != nil {
		return fmt.Errorf("generate rsa key: %w", err)
	}

	uniqueHosts := normalizedCertificateHosts(hosts)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "StockIt Self-Signed Certificate"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedCertificateValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              nil,
		IPAddresses:           nil,
	}

	for _, host := range uniqueHosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, host)
	}

	derCertificate, err := x509.CreateCertificate(nil, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("create x509 certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derCertificate})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})

	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return fmt.Errorf("write certificate: %w", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}

	return nil
}

func tlsHosts(httpAddr, httpsAddr string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	for _, addr := range []string{httpAddr, httpsAddr} {
		if host := listenHost(addr); host != "" {
			hosts = append(hosts, host)
		}
	}
	return normalizedCertificateHosts(hosts)
}

func listenHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.TrimSpace(addr)
	}

	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return ""
	}
	return strings.Trim(host, "[]")
}

func normalizedCertificateHosts(hosts []string) []string {
	seen := map[string]struct{}{}
	normalized := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(strings.Trim(host, "[]"))
		if host == "" || host == "0.0.0.0" || host == "::" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		normalized = append(normalized, host)
	}
	slices.Sort(normalized)
	return normalized
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
