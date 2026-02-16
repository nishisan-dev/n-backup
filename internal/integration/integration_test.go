// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

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

const testStorageName = "e2e-storage"
const testBackupName = "e2e-backup"

// TestEndToEnd_FullBackupSession testa o fluxo completo:
// Agent conecta → Handshake (com storage) → Stream tar.gz → Trailer → Server valida → Commit → Rotação
func TestEndToEnd_FullBackupSession(t *testing.T) {
	pkiDir := t.TempDir()
	storageDir := t.TempDir()
	agentName := "test-agent-e2e"
	pki := generatePKI(t, pkiDir, agentName)

	serverCfg := &config.ServerConfig{
		Storages: map[string]config.StorageInfo{
			testStorageName: {BaseDir: storageDir, MaxBackups: 3},
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

	logger := testLogger()
	go server.RunWithListener(ctx, ln, serverCfg, logger)

	sourceDir := t.TempDir()
	createTestFiles(t, sourceDir)

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

	// 1. Handshake com storage name
	if err := protocol.WriteHandshake(conn, agentName, testStorageName, testBackupName, "v1.2.3"); err != nil {
		t.Fatalf("WriteHandshake: %v", err)
	}

	ack, err := protocol.ReadACK(conn)
	if err != nil {
		t.Fatalf("ReadACK: %v", err)
	}

	if ack.Status != protocol.StatusGo {
		t.Fatalf("expected StatusGo, got %d: %s", ack.Status, ack.Message)
	}

	// 2. Envia byte discriminador single-stream (0x00)
	if _, err := conn.Write([]byte{0x00}); err != nil {
		t.Fatalf("writing single-stream marker: %v", err)
	}

	// 3. Stream tar.gz
	var streamBuf bytes.Buffer
	hasher := sha256.New()
	multiW := io.MultiWriter(&streamBuf, hasher)

	gzW, _ := gzip.NewWriterLevel(multiW, gzip.BestSpeed)
	tw := tar.NewWriter(gzW)

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

	if _, err := conn.Write(streamBuf.Bytes()); err != nil {
		t.Fatalf("writing stream: %v", err)
	}

	if err := protocol.WriteTrailer(conn, checksum, size); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	conn.CloseWrite()

	// 3. Final ACK
	finalACK, err := protocol.ReadFinalACK(conn)
	if err != nil {
		t.Fatalf("ReadFinalACK: %v", err)
	}

	if finalACK.Status != protocol.FinalStatusOK {
		t.Fatalf("expected FinalStatusOK, got %d", finalACK.Status)
	}

	// 4. Verifica backup gravado
	backupDir := filepath.Join(storageDir, agentName, testBackupName)
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("reading backup dir: %v", err)
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

	backupPath := filepath.Join(backupDir, backupFiles[0])
	verifyTarGz(t, backupPath, sourceDir)
}

// TestEndToEnd_StorageNotFound testa que o server rejeita storage inexistente.
func TestEndToEnd_StorageNotFound(t *testing.T) {
	pkiDir := t.TempDir()
	pki := generatePKI(t, pkiDir, "some-agent")

	serverCfg := &config.ServerConfig{
		Storages: map[string]config.StorageInfo{
			"existing": {BaseDir: t.TempDir(), MaxBackups: 3},
		},
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

	// Envia handshake com storage que não existe
	if err := protocol.WriteHandshake(conn, "some-agent", "nonexistent-storage", "some-backup", "v1.2.3"); err != nil {
		t.Fatalf("WriteHandshake: %v", err)
	}

	ack, err := protocol.ReadACK(conn)
	if err != nil {
		t.Fatalf("ReadACK: %v", err)
	}

	if ack.Status != protocol.StatusStorageNotFound {
		t.Errorf("expected StatusStorageNotFound, got %d: %s", ack.Status, ack.Message)
	}
}

// TestEndToEnd_HealthCheck testa o fluxo de health check.
func TestEndToEnd_HealthCheck(t *testing.T) {
	pkiDir := t.TempDir()
	pki := generatePKI(t, pkiDir, "health-check")

	serverCfg := &config.ServerConfig{
		Storages: map[string]config.StorageInfo{
			"default": {BaseDir: t.TempDir(), MaxBackups: 3},
		},
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

// TestEndToEnd_BusyLock testa que o server rejeita backup duplicado do mesmo agent:storage.
func TestEndToEnd_BusyLock(t *testing.T) {
	pkiDir := t.TempDir()
	storageDir := t.TempDir()
	agentName := "busy-agent"
	pki := generatePKI(t, pkiDir, agentName)

	serverCfg := &config.ServerConfig{
		Storages: map[string]config.StorageInfo{
			testStorageName: {BaseDir: storageDir, MaxBackups: 3},
		},
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

	protocol.WriteHandshake(conn1, agentName, testStorageName, testBackupName, "v1.2.3")
	ack1, _ := protocol.ReadACK(conn1)
	if ack1.Status != protocol.StatusGo {
		t.Fatalf("expected GO for conn1, got %d", ack1.Status)
	}

	// Segunda conexão: deve receber BUSY
	time.Sleep(100 * time.Millisecond)

	conn2, err := tls.Dial("tcp", ln.Addr().String(), clientTLSCfg)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.Close()

	protocol.WriteHandshake(conn2, agentName, testStorageName, testBackupName, "v1.2.3")
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

	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("2026-02-%02dT02-00-00.tar.gz", i)
		os.WriteFile(filepath.Join(agentDir, name), []byte("data"), 0644)
	}

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

// TestEndToEnd_ParallelBackupSession testa o fluxo paralelo:
// Agent → Handshake → ACK GO → ParallelInit → ParallelJoin(stream 0) → Stream data → Trailer (primary) → FinalACK → Commit
func TestEndToEnd_ParallelBackupSession(t *testing.T) {
	pkiDir := t.TempDir()
	storageDir := t.TempDir()
	agentName := "test-agent-parallel"
	pki := generatePKI(t, pkiDir, agentName)

	serverCfg := &config.ServerConfig{
		Storages: map[string]config.StorageInfo{
			testStorageName: {BaseDir: storageDir, MaxBackups: 3},
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

	logger := testLogger()
	go server.RunWithListener(ctx, ln, serverCfg, logger)

	sourceDir := t.TempDir()
	createTestFiles(t, sourceDir)

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

	// Conn primária: control-only (Handshake + ParallelInit + Trailer + FinalACK)
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLSCfg)
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer conn.Close()

	// 1. Handshake
	if err := protocol.WriteHandshake(conn, agentName, testStorageName, testBackupName, "v1.2.3"); err != nil {
		t.Fatalf("WriteHandshake: %v", err)
	}

	ack, err := protocol.ReadACK(conn)
	if err != nil {
		t.Fatalf("ReadACK: %v", err)
	}

	if ack.Status != protocol.StatusGo {
		t.Fatalf("expected StatusGo, got %d: %s", ack.Status, ack.Message)
	}

	sessionID := ack.SessionID // session ID retornado pelo server

	// 2. ParallelInit: 2 streams, 256KB chunks
	if err := protocol.WriteParallelInit(conn, 2, 256*1024); err != nil {
		t.Fatalf("WriteParallelInit: %v", err)
	}

	// 3. Prepara dados tar.gz
	var streamBuf bytes.Buffer
	hasher := sha256.New()
	multiW := io.MultiWriter(&streamBuf, hasher)

	gzW, _ := gzip.NewWriterLevel(multiW, gzip.BestSpeed)
	tw := tar.NewWriter(gzW)

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

	// 4. Conecta stream 0 via ParallelJoin (com retry para tolerar race em CI)
	// O server precisa processar o ParallelInit e registrar a sessão antes do Join.
	var stream0Conn *tls.Conn
	var pAck *protocol.ParallelACK
	for attempt := 0; attempt < 10; attempt++ {
		sc, err := tls.Dial("tcp", ln.Addr().String(), clientTLSCfg)
		if err != nil {
			t.Fatalf("TLS dial stream 0: %v", err)
		}

		if err := protocol.WriteParallelJoin(sc, sessionID, 0); err != nil {
			sc.Close()
			t.Fatalf("WriteParallelJoin stream 0: %v", err)
		}

		ack, err := protocol.ReadParallelACK(sc)
		if err != nil {
			sc.Close()
			t.Fatalf("ReadParallelACK stream 0: %v", err)
		}

		if ack.Status == protocol.ParallelStatusOK {
			stream0Conn = sc
			pAck = ack
			break
		}

		// Session not yet registered — retry after short backoff
		sc.Close()
		time.Sleep(50 * time.Millisecond)
	}
	if stream0Conn == nil {
		t.Fatalf("ParallelJoin failed after retries, last status: %d", pAck.Status)
	}
	defer stream0Conn.Close()

	// 5. Envia dados com ChunkHeader framing pelo stream 0
	chunkSize := 256 * 1024
	rawData := streamBuf.Bytes()
	var globalSeq uint32
	for off := 0; off < len(rawData); {
		end := off + chunkSize
		if end > len(rawData) {
			end = len(rawData)
		}
		chunk := rawData[off:end]

		if err := protocol.WriteChunkHeader(stream0Conn, globalSeq, uint32(len(chunk))); err != nil {
			t.Fatalf("WriteChunkHeader seq %d: %v", globalSeq, err)
		}
		if _, err := stream0Conn.Write(chunk); err != nil {
			t.Fatalf("writing chunk data seq %d: %v", globalSeq, err)
		}
		globalSeq++
		off = end
	}

	// Fecha stream 0 (EOF) — server receiveParallelStream termina
	stream0Conn.CloseWrite()

	// Aguarda server processar antes de enviar trailer
	time.Sleep(200 * time.Millisecond)

	// 6. Envia Trailer direto pela conn primária (sem ChunkHeader framing)
	if err := protocol.WriteTrailer(conn, checksum, size); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	// 7. Final ACK
	finalACK, err := protocol.ReadFinalACK(conn)
	if err != nil {
		t.Fatalf("ReadFinalACK: %v", err)
	}

	if finalACK.Status != protocol.FinalStatusOK {
		t.Fatalf("expected FinalStatusOK, got %d", finalACK.Status)
	}

	// 8. Verifica backup gravado
	backupDir := filepath.Join(storageDir, agentName, testBackupName)
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("reading backup dir: %v", err)
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

	backupPath := filepath.Join(backupDir, backupFiles[0])
	verifyTarGz(t, backupPath, sourceDir)
}

// ===== Helpers =====

type pkiPaths struct {
	caCertPath     string
	serverCertPath string
	serverKeyPath  string
	clientCertPath string
	clientKeyPath  string
}

func generatePKI(t *testing.T, dir string, agentCN string) *pkiPaths {
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

	clientKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: agentCN},
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
