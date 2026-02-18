// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package config

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/nishisan-dev/n-backup/internal/protocol"
	"gopkg.in/yaml.v3"
)

// ServerConfig representa a configuração completa do nbackup-server.
type ServerConfig struct {
	Server       ServerListen           `yaml:"server"`
	TLS          TLSServer              `yaml:"tls"`
	Storages     map[string]StorageInfo `yaml:"storages"`
	Logging      LoggingInfo            `yaml:"logging"`
	FlowRotation FlowRotationConfig     `yaml:"flow_rotation"`
	WebUI        WebUIConfig            `yaml:"web_ui"`
}

// WebUIConfig configura o listener HTTP da SPA de observabilidade.
type WebUIConfig struct {
	Enabled      bool          `yaml:"enabled"`
	Listen       string        `yaml:"listen"`        // default: "127.0.0.1:9848"
	ReadTimeout  time.Duration `yaml:"read_timeout"`  // default: 5s
	WriteTimeout time.Duration `yaml:"write_timeout"` // default: 15s
	IdleTimeout  time.Duration `yaml:"idle_timeout"`  // default: 60s
	AllowOrigins []string      `yaml:"allow_origins"` // IP ou CIDR (deny-by-default)

	// Persistência de eventos
	EventsFile     string `yaml:"events_file"`      // default: "events.jsonl"
	EventsMaxLines int    `yaml:"events_max_lines"` // default: 10000

	// Persistência de histórico de sessões finalizadas
	SessionHistoryFile     string `yaml:"session_history_file"`      // default: "session-history.jsonl"
	SessionHistoryMaxLines int    `yaml:"session_history_max_lines"` // default: 5000

	// Snapshots periódicos de sessões ativas
	ActiveSessionsFile     string        `yaml:"active_sessions_file"`      // default: "active-sessions.jsonl"
	ActiveSessionsMaxLines int           `yaml:"active_sessions_max_lines"` // default: 20000
	ActiveSnapshotInterval time.Duration `yaml:"active_snapshot_interval"`  // default: 5m

	// Parsed é preenchido em validate(); não vem do YAML.
	ParsedCIDRs []*net.IPNet `yaml:"-"`
}

// FlowRotationConfig configura a rotação automática de flows degradados.
// Quando habilitada, o server fecha conexões de streams com throughput abaixo
// do threshold por tempo prolongado, forçando o agent a reconectar com nova source port.
type FlowRotationConfig struct {
	Enabled    bool          `yaml:"enabled"`     // default: false
	MinMBps    float64       `yaml:"min_mbps"`    // threshold em MB/s (default: 1.0)
	EvalWindow time.Duration `yaml:"eval_window"` // janela de avaliação (default: 60m)
	Cooldown   time.Duration `yaml:"cooldown"`    // cooldown entre rotações (default: 15m)
}

// ServerListen contém o endereço de escuta do server.
type ServerListen struct {
	Listen string `yaml:"listen"`
}

// TLSServer contém os caminhos dos certificados mTLS do server.
type TLSServer struct {
	CACert     string `yaml:"ca_cert"`
	ServerCert string `yaml:"server_cert"`
	ServerKey  string `yaml:"server_key"`
}

// StorageInfo contém configurações de armazenamento e rotação de um storage nomeado.
type StorageInfo struct {
	BaseDir                string `yaml:"base_dir"`
	MaxBackups             int    `yaml:"max_backups"`
	AssemblerMode          string `yaml:"assembler_mode"`              // eager|lazy (default: eager)
	AssemblerPendingMem    string `yaml:"assembler_pending_mem_limit"` // ex: "8mb" (default: 8mb)
	AssemblerPendingMemRaw int64  `yaml:"-"`
	CompressionMode        string `yaml:"compression_mode"` // gzip|zst (default: gzip)
}

// CompressionModeByte converte o compression_mode string para a constante de protocolo.
func (s StorageInfo) CompressionModeByte() byte {
	switch s.CompressionMode {
	case "zst":
		return protocol.CompressionZstd
	default:
		return protocol.CompressionGzip
	}
}

// FileExtension retorna a extensão de arquivo para backups deste storage.
func (s StorageInfo) FileExtension() string {
	switch s.CompressionMode {
	case "zst":
		return ".tar.zst"
	default:
		return ".tar.gz"
	}
}

// GetStorage retorna o StorageInfo pelo nome ou false se não existir.
func (c *ServerConfig) GetStorage(name string) (StorageInfo, bool) {
	s, ok := c.Storages[name]
	return s, ok
}

// LoadServerConfig lê e valida o arquivo YAML de configuração do server.
func LoadServerConfig(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading server config: %w", err)
	}

	var cfg ServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing server config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating server config: %w", err)
	}

	return &cfg, nil
}

func (c *ServerConfig) validate() error {
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen is required")
	}
	if c.TLS.CACert == "" {
		return fmt.Errorf("tls.ca_cert is required")
	}
	if c.TLS.ServerCert == "" {
		return fmt.Errorf("tls.server_cert is required")
	}
	if c.TLS.ServerKey == "" {
		return fmt.Errorf("tls.server_key is required")
	}
	if len(c.Storages) == 0 {
		return fmt.Errorf("storages must have at least one entry")
	}
	for name, s := range c.Storages {
		if s.BaseDir == "" {
			return fmt.Errorf("storages.%s.base_dir is required", name)
		}
		if s.MaxBackups < 1 {
			s.MaxBackups = 5
		}

		if s.AssemblerMode == "" {
			s.AssemblerMode = "eager"
		}
		s.AssemblerMode = strings.ToLower(strings.TrimSpace(s.AssemblerMode))
		if s.AssemblerMode != "eager" && s.AssemblerMode != "lazy" {
			return fmt.Errorf("storages.%s.assembler_mode must be eager or lazy, got %q", name, s.AssemblerMode)
		}

		if s.AssemblerPendingMem == "" {
			s.AssemblerPendingMem = "8mb"
		}
		parsed, err := ParseByteSize(s.AssemblerPendingMem)
		if err != nil {
			return fmt.Errorf("storages.%s.assembler_pending_mem_limit: %w", name, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("storages.%s.assembler_pending_mem_limit must be > 0, got %s", name, s.AssemblerPendingMem)
		}
		s.AssemblerPendingMemRaw = parsed

		// Compression mode: default gzip
		if s.CompressionMode == "" {
			s.CompressionMode = "gzip"
		}
		s.CompressionMode = strings.ToLower(strings.TrimSpace(s.CompressionMode))
		if s.CompressionMode != "gzip" && s.CompressionMode != "zst" {
			return fmt.Errorf("storages.%s.compression_mode must be gzip or zst, got %q", name, s.CompressionMode)
		}

		c.Storages[name] = s
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}

	// Flow Rotation defaults
	if c.FlowRotation.Enabled {
		if c.FlowRotation.MinMBps <= 0 {
			c.FlowRotation.MinMBps = 1.0
		}
		if c.FlowRotation.EvalWindow <= 0 {
			c.FlowRotation.EvalWindow = 60 * time.Minute
		}
		if c.FlowRotation.Cooldown <= 0 {
			c.FlowRotation.Cooldown = 15 * time.Minute
		}
	}

	// Web UI defaults e validação
	if c.WebUI.Enabled {
		if c.WebUI.Listen == "" {
			c.WebUI.Listen = "127.0.0.1:9848"
		}
		if c.WebUI.ReadTimeout <= 0 {
			c.WebUI.ReadTimeout = 5 * time.Second
		}
		if c.WebUI.WriteTimeout <= 0 {
			c.WebUI.WriteTimeout = 15 * time.Second
		}
		if c.WebUI.IdleTimeout <= 0 {
			c.WebUI.IdleTimeout = 60 * time.Second
		}
		if c.WebUI.EventsFile == "" {
			c.WebUI.EventsFile = "events.jsonl"
		}
		if c.WebUI.EventsMaxLines <= 0 {
			c.WebUI.EventsMaxLines = 10000
		}
		if c.WebUI.SessionHistoryFile == "" {
			c.WebUI.SessionHistoryFile = "session-history.jsonl"
		}
		if c.WebUI.SessionHistoryMaxLines <= 0 {
			c.WebUI.SessionHistoryMaxLines = 5000
		}
		if c.WebUI.ActiveSessionsFile == "" {
			c.WebUI.ActiveSessionsFile = "active-sessions.jsonl"
		}
		if c.WebUI.ActiveSessionsMaxLines <= 0 {
			c.WebUI.ActiveSessionsMaxLines = 20000
		}
		if c.WebUI.ActiveSnapshotInterval <= 0 {
			c.WebUI.ActiveSnapshotInterval = 5 * time.Minute
		}
		if len(c.WebUI.AllowOrigins) == 0 {
			return fmt.Errorf("web_ui.allow_origins is required when web_ui is enabled (deny-by-default)")
		}
		for _, origin := range c.WebUI.AllowOrigins {
			_, cidr, err := net.ParseCIDR(origin)
			if err != nil {
				// Tenta como IP único → converte para /32 ou /128
				ip := net.ParseIP(strings.TrimSpace(origin))
				if ip == nil {
					return fmt.Errorf("web_ui.allow_origins: %q is not a valid IP or CIDR", origin)
				}
				if ip.To4() != nil {
					_, cidr, _ = net.ParseCIDR(ip.String() + "/32")
				} else {
					_, cidr, _ = net.ParseCIDR(ip.String() + "/128")
				}
			}
			c.WebUI.ParsedCIDRs = append(c.WebUI.ParsedCIDRs, cidr)
		}
	}

	return nil
}
