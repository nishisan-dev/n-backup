package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testPKI contém os caminhos dos certificados gerados para teste.
type testPKI struct {
	CACertPath     string
	ServerCertPath string
	ServerKeyPath  string
	ClientCertPath string
	ClientKeyPath  string
}

// generateTestPKI gera uma PKI completa (CA, server cert, client cert) em um diretório temporário.
func generateTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()

	// Gera a CA
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(1 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating CA certificate: %v", err)
	}

	caCertPath := filepath.Join(dir, "ca.pem")
	writePEM(t, caCertPath, "CERTIFICATE", caCertDER)

	// Gera certificado do server
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating server key: %v", err)
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "Test Server"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		t.Fatalf("parsing CA certificate: %v", err)
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating server certificate: %v", err)
	}

	serverCertPath := filepath.Join(dir, "server.pem")
	writePEM(t, serverCertPath, "CERTIFICATE", serverCertDER)

	serverKeyPath := filepath.Join(dir, "server-key.pem")
	writeKeyPEM(t, serverKeyPath, serverKey)

	// Gera certificado do client
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating client key: %v", err)
	}

	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "Test Agent"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientCertDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating client certificate: %v", err)
	}

	clientCertPath := filepath.Join(dir, "client.pem")
	writePEM(t, clientCertPath, "CERTIFICATE", clientCertDER)

	clientKeyPath := filepath.Join(dir, "client-key.pem")
	writeKeyPEM(t, clientKeyPath, clientKey)

	return &testPKI{
		CACertPath:     caCertPath,
		ServerCertPath: serverCertPath,
		ServerKeyPath:  serverKeyPath,
		ClientCertPath: clientCertPath,
		ClientKeyPath:  clientKeyPath,
	}
}

func writePEM(t *testing.T, path, blockType string, data []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating file %s: %v", path, err)
	}
	defer f.Close()

	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: data}); err != nil {
		t.Fatalf("encoding PEM: %v", err)
	}
}

func writeKeyPEM(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling EC key: %v", err)
	}
	writePEM(t, path, "EC PRIVATE KEY", der)
}

func TestNewClientTLSConfig(t *testing.T) {
	pki := generateTestPKI(t)

	cfg, err := NewClientTLSConfig(pki.CACertPath, pki.ClientCertPath, pki.ClientKeyPath)
	if err != nil {
		t.Fatalf("NewClientTLSConfig: %v", err)
	}

	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected TLS 1.3, got %d", cfg.MinVersion)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("expected 1 certificate, got %d", len(cfg.Certificates))
	}
	if cfg.RootCAs == nil {
		t.Error("expected non-nil RootCAs")
	}
}

func TestNewServerTLSConfig(t *testing.T) {
	pki := generateTestPKI(t)

	cfg, err := NewServerTLSConfig(pki.CACertPath, pki.ServerCertPath, pki.ServerKeyPath)
	if err != nil {
		t.Fatalf("NewServerTLSConfig: %v", err)
	}

	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected TLS 1.3, got %d", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected RequireAndVerifyClientCert, got %d", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("expected non-nil ClientCAs")
	}
}

func TestMTLSConnection(t *testing.T) {
	pki := generateTestPKI(t)

	serverCfg, err := NewServerTLSConfig(pki.CACertPath, pki.ServerCertPath, pki.ServerKeyPath)
	if err != nil {
		t.Fatalf("NewServerTLSConfig: %v", err)
	}

	clientCfg, err := NewClientTLSConfig(pki.CACertPath, pki.ClientCertPath, pki.ClientKeyPath)
	if err != nil {
		t.Fatalf("NewClientTLSConfig: %v", err)
	}

	// Inicia um listener TLS
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("TLS listen: %v", err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()

		// Força o handshake TLS
		tlsConn := conn.(*tls.Conn)
		if err := tlsConn.Handshake(); err != nil {
			done <- err
			return
		}

		// Lê dados e ecoa de volta
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			done <- err
			return
		}
		_, err = conn.Write(buf[:n])
		done <- err
	}()

	// Client
	clientCfg.ServerName = "localhost"
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer conn.Close()

	msg := []byte("hello mTLS")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("writing to TLS conn: %v", err)
	}

	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("reading from TLS conn: %v", err)
	}

	if string(buf[:n]) != string(msg) {
		t.Errorf("expected %q, got %q", msg, buf[:n])
	}

	// Espera o server terminar
	if err := <-done; err != nil {
		t.Fatalf("server error: %v", err)
	}
}

func TestMTLSConnection_InvalidClientCert(t *testing.T) {
	pki := generateTestPKI(t)

	serverCfg, err := NewServerTLSConfig(pki.CACertPath, pki.ServerCertPath, pki.ServerKeyPath)
	if err != nil {
		t.Fatalf("NewServerTLSConfig: %v", err)
	}

	// Gera um client com certificado auto-assinado (NÃO assinado pela CA)
	untrustedKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	untrustedTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(99),
		Subject:      pkix.Name{CommonName: "Untrusted Agent"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	// Auto-assinado (não pela CA)
	untrustedCertDER, _ := x509.CreateCertificate(rand.Reader, untrustedTemplate, untrustedTemplate, &untrustedKey.PublicKey, untrustedKey)

	dir := t.TempDir()
	untrustedCertPath := filepath.Join(dir, "untrusted.pem")
	writePEM(t, untrustedCertPath, "CERTIFICATE", untrustedCertDER)
	untrustedKeyPath := filepath.Join(dir, "untrusted-key.pem")
	writeKeyPEM(t, untrustedKeyPath, untrustedKey)

	clientCfg, err := NewClientTLSConfig(pki.CACertPath, untrustedCertPath, untrustedKeyPath)
	if err != nil {
		t.Fatalf("NewClientTLSConfig: %v", err)
	}

	// Inicia listener
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("TLS listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tlsConn := conn.(*tls.Conn)
		tlsConn.Handshake() // Esperado falhar
	}()

	clientCfg.ServerName = "localhost"
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		// Conexão recusada no dial — OK
		return
	}
	defer conn.Close()

	// Se conseguiu conectar, o handshake ou o write devem falhar
	if _, err := conn.Write([]byte("test")); err == nil {
		// Tenta ler — o server deveria ter fechado a conexão
		buf := make([]byte, 10)
		_, readErr := conn.Read(buf)
		if readErr == nil {
			t.Fatal("expected TLS handshake to fail with untrusted certificate")
		}
	}
}

func TestNewClientTLSConfig_InvalidCACert(t *testing.T) {
	dir := t.TempDir()
	fakeCa := filepath.Join(dir, "fake-ca.pem")
	os.WriteFile(fakeCa, []byte("not a certificate"), 0644)

	pki := generateTestPKI(t)
	_, err := NewClientTLSConfig(fakeCa, pki.ClientCertPath, pki.ClientKeyPath)
	if err == nil {
		t.Fatal("expected error for invalid CA cert")
	}
}

func TestNewClientTLSConfig_MissingFile(t *testing.T) {
	pki := generateTestPKI(t)
	_, err := NewClientTLSConfig(pki.CACertPath, "/nonexistent/client.pem", "/nonexistent/key.pem")
	if err == nil {
		t.Fatal("expected error for missing cert file")
	}
}
