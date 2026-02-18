// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig representa a configuração completa do nbackup-agent.
type AgentConfig struct {
	Agent   AgentInfo     `yaml:"agent"`
	Daemon  DaemonInfo    `yaml:"daemon"`
	Server  ServerAddr    `yaml:"server"`
	TLS     TLSClient     `yaml:"tls"`
	Backups []BackupEntry `yaml:"backups"`
	Retry   RetryInfo     `yaml:"retry"`
	Resume  ResumeConfig  `yaml:"resume"`
	Logging LoggingInfo   `yaml:"logging"`
}

// AgentInfo identifica o agent.
type AgentInfo struct {
	Name string `yaml:"name"`
}

// DaemonInfo contém configurações do modo daemon.
type DaemonInfo struct {
	ControlChannel ControlChannelConfig `yaml:"control_channel"`
}

// ControlChannelConfig configura o canal de controle persistente com o server.
type ControlChannelConfig struct {
	Enabled           *bool         `yaml:"enabled"`             // default: true
	KeepaliveInterval time.Duration `yaml:"keepalive_interval"`  // default: 30s
	ReconnectDelay    time.Duration `yaml:"reconnect_delay"`     // default: 5s
	MaxReconnectDelay time.Duration `yaml:"max_reconnect_delay"` // default: 5m
}

// ServerAddr contém o endereço do servidor de backup.
type ServerAddr struct {
	Address string `yaml:"address"`
}

// TLSClient contém os caminhos dos certificados mTLS do client.
type TLSClient struct {
	CACert     string `yaml:"ca_cert"`
	ClientCert string `yaml:"client_cert"`
	ClientKey  string `yaml:"client_key"`
}

// BackupEntry representa um bloco de backup nomeado com storage de destino.
type BackupEntry struct {
	Name            string         `yaml:"name"`            // Identificador local do backup
	Storage         string         `yaml:"storage"`         // Nome do storage no server
	Schedule        string         `yaml:"schedule"`        // Cron expression individual deste backup
	Sources         []BackupSource `yaml:"sources"`
	Exclude         []string       `yaml:"exclude"`
	Parallels       int            `yaml:"parallels"`       // 0=desabilitado (single stream), 1-255=máx streams paralelos
	DSCP            string         `yaml:"dscp"`            // DSCP marking (ex: "AF41", "EF"), vazio=desabilitado
	AutoScaler      string         `yaml:"auto_scaler"`     // "efficiency" (default) | "adaptive"
	BandwidthLimit  string         `yaml:"bandwidth_limit"` // Limite de upload em Bytes/seg (ex: "50mb", "1gb"), vazio=sem limite
	BandwidthLimitRaw int64        `yaml:"-"`               // valor parseado em bytes/seg
}

// BackupSource representa um diretório de origem para backup.
type BackupSource struct {
	Path string `yaml:"path"`
}

// RetryInfo contém configurações de retry com exponential backoff.
type RetryInfo struct {
	MaxAttempts  int           `yaml:"max_attempts"`
	InitialDelay time.Duration `yaml:"initial_delay"`
	MaxDelay     time.Duration `yaml:"max_delay"`
}

// DefaultChunkSize é o tamanho padrão de cada chunk para streaming paralelo (1MB).
const DefaultChunkSize = 1 * 1024 * 1024

// ResumeConfig contém configurações do ring buffer para resume.
type ResumeConfig struct {
	BufferSize    string `yaml:"buffer_size"` // ex: "256mb", "1gb"
	BufferSizeRaw int64  `yaml:"-"`           // valor parseado em bytes
	ChunkSize     string `yaml:"chunk_size"`  // ex: "1mb", "4mb" (default: 1mb)
	ChunkSizeRaw  int64  `yaml:"-"`           // valor parseado em bytes
}

// LoggingInfo contém configurações de logging.
type LoggingInfo struct {
	Level       string `yaml:"level"`
	Format      string `yaml:"format"`
	File        string `yaml:"file"`         // Caminho para arquivo de log (ex: /var/log/nbackup/agent.log)
	StreamStats bool   `yaml:"stream_stats"` // Habilita stats por stream em sessões paralelas (padrão: false)
}

// LoadAgentConfig lê e valida o arquivo YAML de configuração do agent.
func LoadAgentConfig(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading agent config: %w", err)
	}

	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing agent config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating agent config: %w", err)
	}

	return &cfg, nil
}

func (c *AgentConfig) validate() error {
	if c.Agent.Name == "" {
		return fmt.Errorf("agent.name is required")
	}

	if c.Server.Address == "" {
		return fmt.Errorf("server.address is required")
	}
	if c.TLS.CACert == "" {
		return fmt.Errorf("tls.ca_cert is required")
	}
	if c.TLS.ClientCert == "" {
		return fmt.Errorf("tls.client_cert is required")
	}
	if c.TLS.ClientKey == "" {
		return fmt.Errorf("tls.client_key is required")
	}
	if len(c.Backups) == 0 {
		return fmt.Errorf("backups must have at least one entry")
	}
	for i, b := range c.Backups {
		if b.Name == "" {
			return fmt.Errorf("backups[%d].name is required", i)
		}
		if b.Storage == "" {
			return fmt.Errorf("backups[%d].storage is required", i)
		}
		if len(b.Sources) == 0 {
			return fmt.Errorf("backups[%d].sources must have at least one entry", i)
		}
		for j, src := range b.Sources {
			if src.Path == "" {
				return fmt.Errorf("backups[%d].sources[%d].path is required", i, j)
			}
		}
		if b.Schedule == "" {
			return fmt.Errorf("backups[%d].schedule is required", i)
		}
		if b.Parallels < 0 || b.Parallels > 255 {
			return fmt.Errorf("backups[%d].parallels must be between 0 and 255, got %d", i, b.Parallels)
		}
		if b.DSCP != "" {
			dscp := strings.TrimSpace(strings.ToUpper(b.DSCP))
			validDSCP := map[string]bool{
				"EF":   true,
				"AF11": true, "AF12": true, "AF13": true,
				"AF21": true, "AF22": true, "AF23": true,
				"AF31": true, "AF32": true, "AF33": true,
				"AF41": true, "AF42": true, "AF43": true,
				"CS0": true, "CS1": true, "CS2": true, "CS3": true,
				"CS4": true, "CS5": true, "CS6": true, "CS7": true,
			}
			if !validDSCP[dscp] {
				return fmt.Errorf("backups[%d].dscp: unknown value %q (valid: EF, AF11..AF43, CS0..CS7)", i, b.DSCP)
			}
		}
		// Auto-scaler mode validation
		switch strings.ToLower(strings.TrimSpace(b.AutoScaler)) {
		case "", "efficiency":
			c.Backups[i].AutoScaler = "efficiency"
		case "adaptive":
			c.Backups[i].AutoScaler = "adaptive"
		default:
			return fmt.Errorf("backups[%d].auto_scaler: unknown value %q (valid: efficiency, adaptive)", i, b.AutoScaler)
		}
		// Bandwidth limit validation
		if b.BandwidthLimit != "" {
			bwParsed, err := ParseByteSize(b.BandwidthLimit)
			if err != nil {
				return fmt.Errorf("backups[%d].bandwidth_limit: %w", i, err)
			}
			if bwParsed < 64*1024 {
				return fmt.Errorf("backups[%d].bandwidth_limit must be at least 64kb, got %s", i, b.BandwidthLimit)
			}
			c.Backups[i].BandwidthLimitRaw = bwParsed
		}
	}
	if c.Retry.MaxAttempts <= 0 {
		c.Retry.MaxAttempts = 5
	}
	if c.Retry.InitialDelay <= 0 {
		c.Retry.InitialDelay = 1 * time.Second
	}
	if c.Retry.MaxDelay <= 0 {
		c.Retry.MaxDelay = 5 * time.Minute
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}

	// Resume defaults
	if c.Resume.BufferSize == "" {
		c.Resume.BufferSize = "256mb"
	}
	parsed, err := ParseByteSize(c.Resume.BufferSize)
	if err != nil {
		return fmt.Errorf("resume.buffer_size: %w", err)
	}
	c.Resume.BufferSizeRaw = parsed

	// Chunk size defaults
	if c.Resume.ChunkSize == "" {
		c.Resume.ChunkSize = "1mb"
	}
	chunkParsed, err := ParseByteSize(c.Resume.ChunkSize)
	if err != nil {
		return fmt.Errorf("resume.chunk_size: %w", err)
	}
	if chunkParsed < 64*1024 {
		return fmt.Errorf("resume.chunk_size must be at least 64kb, got %s", c.Resume.ChunkSize)
	}
	if chunkParsed > 16*1024*1024 {
		return fmt.Errorf("resume.chunk_size must be at most 16mb, got %s", c.Resume.ChunkSize)
	}
	c.Resume.ChunkSizeRaw = chunkParsed

	// Control channel defaults
	cc := &c.Daemon.ControlChannel
	if cc.Enabled == nil {
		defaultEnabled := true
		cc.Enabled = &defaultEnabled
	}
	if cc.KeepaliveInterval <= 0 {
		cc.KeepaliveInterval = 30 * time.Second
	}
	if cc.KeepaliveInterval < time.Second {
		return fmt.Errorf("daemon.control_channel.keepalive_interval must be >= 1s, got %s", cc.KeepaliveInterval)
	}
	if cc.ReconnectDelay <= 0 {
		cc.ReconnectDelay = 5 * time.Second
	}
	if cc.MaxReconnectDelay <= 0 {
		cc.MaxReconnectDelay = 5 * time.Minute
	}
	if cc.MaxReconnectDelay < cc.ReconnectDelay {
		cc.MaxReconnectDelay = cc.ReconnectDelay
	}

	return nil
}

// ParseByteSize converte strings human-readable como "256mb", "1gb" para bytes.
func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	// Ordenado do sufixo mais longo para o mais curto
	// para evitar que "mb" matche como "b"
	type suffix struct {
		s string
		m int64
	}
	suffixes := []suffix{
		{"gb", 1024 * 1024 * 1024},
		{"mb", 1024 * 1024},
		{"kb", 1024},
		{"b", 1},
	}

	for _, sfx := range suffixes {
		if strings.HasSuffix(s, sfx.s) {
			numStr := strings.TrimSuffix(s, sfx.s)
			num, err := strconv.ParseInt(numStr, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid number %q: %w", numStr, err)
			}
			return num * sfx.m, nil
		}
	}

	// Tenta interpretar como número puro (bytes)
	num, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unknown size format %q", s)
	}
	return num, nil
}
