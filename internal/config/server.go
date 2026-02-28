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
	GapDetection GapDetectionConfig     `yaml:"gap_detection"`
	WebUI        WebUIConfig            `yaml:"web_ui"`
	ChunkBuffer  ChunkBufferConfig      `yaml:"chunk_buffer"`
}

// ChunkBufferConfig define o buffer de chunks em memória compartilhado globalmente
// entre todas as sessões de backup paralelo.
// Quando Size for "0" ou vazio, o buffer é desabilitado e o comportamento atual
// é preservado sem qualquer alteração de caminho crítico.
type ChunkBufferConfig struct {
	// Size define a memória máxima alocada para o buffer.
	// "0" ou vazio desabilita o buffer. Aceita sufixos: kb, mb, gb.
	Size string `yaml:"size"` // ex: "64mb", "256mb"

	// DrainRatio define o limiar de ocupação (0.0 a 1.0) a partir do qual
	// o buffer aciona a drenagem para o assembler.
	// nil (campo ausente no YAML) → default 0.5.
	// 0.0 → write-through explícito (drena após cada chunk).
	// 1.0 → drena apenas quando o buffer está completamente cheio.
	// O comportamento de escrita em disco segue o assembler_mode do storage.
	DrainRatio *float64 `yaml:"drain_ratio"` // ptr: nil=default 0.5 | &0.0=write-through

	// ChannelSlots define o número de slots do canal interno do buffer.
	// 0 (ou campo ausente) → auto: sizeRaw / 1MB (compatibilidade retroativa).
	// Útil para ajustar quando chunk_size no agent for menor que 1MB:
	// ex: chunk_size=256KB com size=64MB → channel_slots: 256 (vs. 64 auto).
	ChannelSlots int `yaml:"channel_slots"` // 0 = auto

	// SizeRaw e DrainRatioRaw são preenchidos por validate(); não vêm do YAML.
	SizeRaw       int64   `yaml:"-"`
	DrainRatioRaw float64 `yaml:"-"`
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

	// Intervalo de refresh dos dados de storage (disco + contagem de backups)
	StorageScanInterval time.Duration `yaml:"storage_scan_interval"` // default: 1h, mínimo: 30s

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

// GapDetectionConfig configura a detecção proativa de chunks faltantes
// e o envio de NACKs para retransmissão pelo agent.
type GapDetectionConfig struct {
	Enabled          bool          `yaml:"enabled"`             // default: true (habilitado por padrão)
	Timeout          time.Duration `yaml:"timeout"`             // tempo para gap persistir antes de NACK (default: 60s)
	InFlightTimeout  time.Duration `yaml:"in_flight_timeout"`   // tempo máximo sem progresso de payload de um chunk em voo (default: 30s)
	CheckInterval    time.Duration `yaml:"check_interval"`      // intervalo entre checks de gap (default: 5s)
	MaxNACKsPerCycle int           `yaml:"max_nacks_per_cycle"` // máximo de NACKs por check (default: 5)
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
	CompressionMode        string `yaml:"compression_mode"`   // gzip|zst (default: gzip)
	ChunkShardLevels       int    `yaml:"chunk_shard_levels"` // 1|2 (default: 1, número de níveis de sharding de chunks)
	ChunkFsync             bool   `yaml:"chunk_fsync"`        // fsync nos writes de chunk staging (default: false)
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

		// Chunk shard levels: default 1
		if s.ChunkShardLevels == 0 {
			s.ChunkShardLevels = 1
		}
		if s.ChunkShardLevels < 1 || s.ChunkShardLevels > 2 {
			return fmt.Errorf("storages.%s.chunk_shard_levels must be 1 or 2, got %d", name, s.ChunkShardLevels)
		}

		c.Storages[name] = s
	}

	// Chunk Buffer global
	if c.ChunkBuffer.Size == "" || c.ChunkBuffer.Size == "0" {
		c.ChunkBuffer.SizeRaw = 0 // desabilitado
	} else {
		parsed, err := ParseByteSize(c.ChunkBuffer.Size)
		if err != nil {
			return fmt.Errorf("chunk_buffer.size: %w", err)
		}
		if parsed <= 0 {
			return fmt.Errorf("chunk_buffer.size must be > 0 or \"0\" to disable, got %s", c.ChunkBuffer.Size)
		}
		c.ChunkBuffer.SizeRaw = parsed
		// DrainRatio: nil significia campo ausente no YAML → default 0.5.
		// &0.0 é write-through explícito; &1.0 = só drena quando cheio.
		if c.ChunkBuffer.DrainRatio == nil {
			v := 0.5
			c.ChunkBuffer.DrainRatio = &v
		}
		if *c.ChunkBuffer.DrainRatio < 0 || *c.ChunkBuffer.DrainRatio > 1 {
			return fmt.Errorf("chunk_buffer.drain_ratio must be between 0.0 and 1.0, got %.2f", *c.ChunkBuffer.DrainRatio)
		}
		c.ChunkBuffer.DrainRatioRaw = *c.ChunkBuffer.DrainRatio
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

	// Gap Detection defaults (habilitado por padrão)
	if !c.GapDetection.Enabled {
		// Se não explicitamente habilitado no YAML, habilita com defaults.
		// Para desabilitar, o operador deve setar enabled: false explicitamente.
		// Usamos um truque: se Timeout == 0, significa que nada foi configurado.
		if c.GapDetection.Timeout == 0 && c.GapDetection.CheckInterval == 0 {
			c.GapDetection.Enabled = true
		}
	}
	if c.GapDetection.Enabled {
		if c.GapDetection.Timeout <= 0 {
			c.GapDetection.Timeout = 60 * time.Second
		}
		if c.GapDetection.InFlightTimeout <= 0 {
			c.GapDetection.InFlightTimeout = 30 * time.Second
		}
		if c.GapDetection.CheckInterval <= 0 {
			c.GapDetection.CheckInterval = 5 * time.Second
		}
		if c.GapDetection.MaxNACKsPerCycle <= 0 {
			c.GapDetection.MaxNACKsPerCycle = 5
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
		if c.WebUI.StorageScanInterval <= 0 {
			c.WebUI.StorageScanInterval = 1 * time.Hour
		}
		if c.WebUI.StorageScanInterval < 30*time.Second {
			c.WebUI.StorageScanInterval = 30 * time.Second
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
