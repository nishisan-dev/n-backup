package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nishisan-dev/n-backup/internal/config"
	"github.com/nishisan-dev/n-backup/internal/protocol"
	"github.com/nishisan-dev/n-backup/internal/server"
)

// TestEndToEnd_FullBackupSession testa o fluxo completo:
// Agent conecta → Handshake → Stream tar.gz → Trailer → Server valida checksum → Commit → Rotação
func TestEndToEnd_FullBackupSession(t *testing.T) {
	// Setup PKI
	pkiDir := t.TempDir()
	pki := generatePKI(t, pkiDir)

	// Setup storage
	storageDir := t.TempDir()
	agentName := "test-agent-e2e"

	// Inicia server
	serverCfg := &config.ServerConfig{
		Storage: config.StorageInfo{
			BaseDir:    storageDir,
			MaxBackups: 3,
		},
		Logging: config.LoggingInfo{Level: "debug", Format: "text"},
	}

	serverTLS, err := tls.LoadX509KeyPair(pki.serverCertPath, pki.serverKeyPath)
	if err != nil {
		t.Fatalf("loading server cert: %v", err)
	}

	caPool := loadCAPool(t, pki.caCertPath)

	serverTLSCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverTLS},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSCfg)
	if err != nil {
		t.Fatalf("TLS listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Server em goroutine
	logger := testLogger()
	go server.RunWithListener(ctx, ln, serverCfg, logger)

	// Prepara dados de teste
	sourceDir := t.TempDir()
	createTestFiles(t, sourceDir)

	// --- Client flow ---
	clientTLS, err := tls.LoadX509KeyPair(pki.clientCertPath, pki.clientKeyPath)
	if err != nil {
		t.Fatalf("loading client cert: %v", err)
	}

	clientTLSCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{clientTLS},
		RootCAs:      caPool,
		ServerName:   "localhost",
	}

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLSCfg)
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer conn.Close()

	// 1. Handshake
	if err := protocol.WriteHandshake(conn, agentName); err != nil {
		t.Fatalf("WriteHandshake: %v", err)
	}

	ack, err := protocol.ReadACK(conn)
	if err != nil {
		t.Fatalf("ReadACK: %v", err)
	}

	if ack.Status != protocol.StatusGo {
		t.Fatalf("expected StatusGo, got %d: %s", ack.Status, ack.Message)
	}

	// 2. Stream tar.gz
	var streamBuf bytes.Buffer
	hasher := sha256.New()
	multiW := io.MultiWriter(&streamBuf, hasher)

	gzW, _ := gzip.NewWriterLevel(multiW, gzip.BestSpeed)
	tw := tar.NewWriter(gzW)

	// Walk source dir → tar
	filepath.WalkDir(sourceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, _ := d.Info()

		header, _ := tar.FileInfoHeader(info, "")
		relPath, _ := filepath.Rel(sourceDir, path)
		header.Name = relPath

		tw.WriteHeader(header)

		if info.Mode().IsRegular() {
			f, _ := os.Open(path)
			io.Copy(tw, f)
			f.Close()
		}
		return nil
	})

	tw.Close()
	gzW.Close()

	var checksum [32]byte
	copy(checksum[:], hasher.Sum(nil))
	size := uint64(streamBuf.Len())

	// Envia stream + trailer
	if _, err := conn.Write(streamBuf.Bytes()); err != nil {
		t.Fatalf("writing stream: %v", err)
	}

	if err := protocol.WriteTrailer(conn, checksum, size); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	// Fecha escrita (half-close) para o server receber EOF
	conn.CloseWrite()

	// 3. Lê Final ACK
	finalACK, err := protocol.ReadFinalACK(conn)
	if err != nil {
		t.Fatalf("ReadFinalACK: %v", err)
	}

	if finalACK.Status != protocol.FinalStatusOK {
		t.Fatalf("expected FinalStatusOK, got %d", finalACK.Status)
	}

	// 4. Verifica que o backup foi gravado
	agentDir := filepath.Join(storageDir, agentName)
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatalf("reading agent dir: %v", err)
	}

	var backupFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			backupFiles = append(backupFiles, e.Name())
		}
	}

	if len(backupFiles) != 1 {
		t.Fatalf("expected 1 backup, got %d: %v", len(backupFiles), backupFiles)
	}

	// 5. Verifica que o tar.gz é válido e contém os arquivos
	backupPath := filepath.Join(agentDir, backupFiles[0])
	verifyTarGz(t, backupPath, sourceDir)
}

// TestEndToEnd_HealthCheck testa o fluxo de health check.
func TestEndToEnd_HealthCheck(t *testing.T) {
	pkiDir := t.TempDir()
	pki := generatePKI(t, pkiDir)

	serverCfg := &config.ServerConfig{
		Storage: config.StorageInfo{BaseDir: t.TempDir(), MaxBackups: 3},
		Logging: config.LoggingInfo{Level: "debug", Format: "text"},
	}

	serverTLS, _ := tls.LoadX509KeyPair(pki.serverCertPath, pki.serverKeyPath)
	caPool := loadCAPool(t, pki.caCertPath)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverTLS},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	})
	if err != nil {
		t.Fatalf("TLS listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go server.RunWithListener(ctx, ln, serverCfg, testLogger())

	clientTLS, _ := tls.LoadX509KeyPair(pki.clientCertPath, pki.clientKeyPath)
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{clientTLS},
		RootCAs:      caPool,
		ServerName:   "localhost",
	})
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer conn.Close()

	// PING
	if err := protocol.WritePing(conn); err != nil {
		t.Fatalf("WritePing: %v", err)
	}

	resp, err := protocol.ReadHealthResponse(conn)
	if err != nil {
		t.Fatalf("ReadHealthResponse: %v", err)
	}

	if resp.Status != protocol.HealthStatusReady {
		t.Errorf("expected HealthStatusReady, got %d", resp.Status)
	}
}

// TestEndToEnd_BusyLock testa que o server rejeita backup duplicado do mesmo agent.
func TestEndToEnd_BusyLock(t *testing.T) {
	pkiDir := t.TempDir()
	pki := generatePKI(t, pkiDir)
	storageDir := t.TempDir()
	agentName := "busy-agent"

	serverCfg := &config.ServerConfig{
		Storage: config.StorageInfo{BaseDir: storageDir, MaxBackups: 3},
		Logging: config.LoggingInfo{Level: "debug", Format: "text"},
	}

	serverTLS, _ := tls.LoadX509KeyPair(pki.serverCertPath, pki.serverKeyPath)
	caPool := loadCAPool(t, pki.caCertPath)

	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverTLS},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}

	ln, _ := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go server.RunWithListener(ctx, ln, serverCfg, testLogger())

	clientTLSCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{func() tls.Certificate { c, _ := tls.LoadX509KeyPair(pki.clientCertPath, pki.clientKeyPath); return c }()},
		RootCAs:      caPool,
		ServerName:   "localhost",
	}

	// Primeira conexão: envia handshake mas NÃO fecha (mantém busy)
	conn1, err := tls.Dial("tcp", ln.Addr().String(), clientTLSCfg)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer conn1.Close()

	protocol.WriteHandshake(conn1, agentName)
	ack1, _ := protocol.ReadACK(conn1)
	if ack1.Status != protocol.StatusGo {
		t.Fatalf("expected GO for conn1, got %d", ack1.Status)
	}

	// Segunda conexão: deve receber BUSY
	time.Sleep(100 * time.Millisecond) // garante que o server processou o lock

	conn2, err := tls.Dial("tcp", ln.Addr().String(), clientTLSCfg)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.Close()

	protocol.WriteHandshake(conn2, agentName)
	ack2, err := protocol.ReadACK(conn2)
	if err != nil {
		t.Fatalf("ReadACK conn2: %v", err)
	}

	if ack2.Status != protocol.StatusBusy {
		t.Errorf("expected StatusBusy for second conn, got %d: %s", ack2.Status, ack2.Message)
	}
}

// TestEndToEnd_Rotation testa que a rotação remove backups excedentes.
func TestEndToEnd_Rotation(t *testing.T) {
	storageDir := t.TempDir()
	agentDir := filepath.Join(storageDir, "rotation-agent")
	os.MkdirAll(agentDir, 0755)

	// Cria 5 backups existentes
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("2026-02-%02dT02-00-00.tar.gz", i)
		os.WriteFile(filepath.Join(agentDir, name), []byte("data"), 0644)
	}

	// Rotação com max 3
	if err := server.Rotate(agentDir, 3); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	entries, _ := os.ReadDir(agentDir)
	if len(entries) != 3 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected 3 backups after rotation, got %d: %v", len(entries), names)
	}
}

// ===== Helpers =====

type pkiPaths struct {
	caCertPath     string
	serverCertPath string
	serverKeyPath  string
	clientCertPath string
	clientKeyPath  string
}

func generatePKI(t *testing.T, dir string) *pkiPaths {
	t.Helper()

	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "E2E Test CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(1 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caCertDER, _ := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caCertDER)

	caCertPath := filepath.Join(dir, "ca.pem")
	writePEMFile(t, caCertPath, "CERTIFICATE", caCertDER)

	// Server cert
	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "E2E Test Server"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	serverCertDER, _ := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	serverCertPath := filepath.Join(dir, "server.pem")
	writePEMFile(t, serverCertPath, "CERTIFICATE", serverCertDER)
	serverKeyPath := filepath.Join(dir, "server-key.pem")
	writeECKeyPEM(t, serverKeyPath, serverKey)

	// Client cert
	clientKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "E2E Test Agent"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientCertDER, _ := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	clientCertPath := filepath.Join(dir, "client.pem")
	writePEMFile(t, clientCertPath, "CERTIFICATE", clientCertDER)
	clientKeyPath := filepath.Join(dir, "client-key.pem")
	writeECKeyPEM(t, clientKeyPath, clientKey)

	return &pkiPaths{
		caCertPath:     caCertPath,
		serverCertPath: serverCertPath,
		serverKeyPath:  serverKeyPath,
		clientCertPath: clientCertPath,
		clientKeyPath:  clientKeyPath,
	}
}

func writePEMFile(t *testing.T, path, blockType string, data []byte) {
	t.Helper()
	f, _ := os.Create(path)
	defer f.Close()
	pem.Encode(f, &pem.Block{Type: blockType, Bytes: data})
}

func writeECKeyPEM(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, _ := x509.MarshalECPrivateKey(key)
	writePEMFile(t, path, "EC PRIVATE KEY", der)
}

func loadCAPool(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	data, _ := os.ReadFile(path)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(data)
	return pool
}

func createTestFiles(t *testing.T, dir string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("hello world"), 0644)
	os.WriteFile(filepath.Join(dir, "file2.txt"), bytes.Repeat([]byte("A"), 1024), 0644)
	sub := filepath.Join(dir, "subdir")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "nested.txt"), []byte("nested content"), 0644)
}

func verifyTarGz(t *testing.T, backupPath, sourceDir string) {
	t.Helper()

	f, err := os.Open(backupPath)
	if err != nil {
		t.Fatalf("opening backup: %v", err)
	}
	defer f.Close()

	gzR, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gzR.Close()

	tr := tar.NewReader(gzR)
	var files []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		files = append(files, hdr.Name)
	}

	if len(files) == 0 {
		t.Fatal("backup tar.gz contains no files")
	}

	// Verifica que pelo menos file1.txt está no archive
	found := false
	for _, f := range files {
		if strings.Contains(f, "file1.txt") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("file1.txt not found in backup, files: %v", files)
	}
}

func testLogger() *slog.Logger {
	return slog.Default()
}
