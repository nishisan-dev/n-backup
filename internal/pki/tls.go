// Package pki fornece funções para configuração de TLS com mTLS
// (Mutual TLS) para o protocolo NBackup.
package pki

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// NewClientTLSConfig cria uma configuração TLS 1.3 para o client (agent)
// com autenticação mútua (mTLS).
func NewClientTLSConfig(caCertPath, clientCertPath, clientKeyPath string) (*tls.Config, error) {
	// Carrega o certificado do client
	cert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading client certificate: %w", err)
	}

	// Carrega a CA para validar o server
	caPool, err := loadCACertPool(caCertPath)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
	}, nil
}

// NewServerTLSConfig cria uma configuração TLS 1.3 para o server
// com autenticação mútua obrigatória (mTLS).
func NewServerTLSConfig(caCertPath, serverCertPath, serverKeyPath string) (*tls.Config, error) {
	// Carrega o certificado do server
	cert, err := tls.LoadX509KeyPair(serverCertPath, serverKeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading server certificate: %w", err)
	}

	// Carrega a CA para validar os clients
	caPool, err := loadCACertPool(caCertPath)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}

func loadCACertPool(caCertPath string) (*x509.CertPool, error) {
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA certificate: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to parse CA certificate from %s", caCertPath)
	}

	return pool, nil
}
