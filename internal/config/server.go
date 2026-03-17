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

// GapDetectionConfig is DEPRECATED since v3.0.0.
// ChunkSACK per-chunk acknowledgment replaces gap detection.
// Struct is kept for YAML backward compatibility; all fields are ignored at runtime.
type GapDetectionConfig struct {
	Enabled          bool          `yaml:"enabled"`
	Timeout          time.Duration `yaml:"timeout"`
	InFlightTimeout  time.Duration `yaml:"in_flight_timeout"`
	CheckInterval    time.Duration `yaml:"check_interval"`
	MaxNACKsPerCycle int           `yaml:"max_nacks_per_cycle"`
	enabledSet       bool          `yaml:"-"`
}

// WarnDeprecated emits a warning log if the user explicitly configured gap_detection.
func (c *ServerConfig) WarnDeprecated(logger interface{ Warn(msg string, args ...any) }) {
	if c.GapDetection.enabledSet {
		logger.Warn("gap_detection configuration is deprecated and will be ignored — ChunkSACK per-chunk acknowledgment replaces gap detection since v3.0.0")
	}
}

// UnmarshalYAML preserva a diferença entre campo ausente e enabled: false
// explícito, para que validate() consiga aplicar o default corretamente.
func (g *GapDetectionConfig) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Enabled          *bool         `yaml:"enabled"`
		Timeout          time.Duration `yaml:"timeout"`
		InFlightTimeout  time.Duration `yaml:"in_flight_timeout"`
		CheckInterval    time.Duration `yaml:"check_interval"`
		MaxNACKsPerCycle int           `yaml:"max_nacks_per_cycle"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}

	if raw.Enabled != nil {
		g.Enabled = *raw.Enabled
		g.enabledSet = true
	} else {
		g.Enabled = false
		g.enabledSet = false
	}
	g.Timeout = raw.Timeout
	g.InFlightTimeout = raw.InFlightTimeout
	g.CheckInterval = raw.CheckInterval
	g.MaxNACKsPerCycle = raw.MaxNACKsPerCycle

	return nil
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

// BucketMode define os modos de operação do object storage pós-commit.
const (
	BucketModeSync    = "sync"    // espelha 1:1 o storage local (upload + delete espelhado)
	BucketModeOffload = "offload" // envia para o bucket e apaga o local
	BucketModeArchive = "archive" // envia para o bucket apenas backups deletados pelo Rotate
)

// SyncStrategy define a ordem de operações no modo sync.
const (
	SyncStrategySafe           = "safe"             // default: upload primeiro, delete depois
	SyncStrategySpaceEfficient = "space_efficient" // delete primeiro, upload depois (economia de espaço)
)

// BucketCredentials referencia credenciais via variáveis de ambiente.
// Nunca armazena valores diretamente — segurança por padrão.
type BucketCredentials struct {
	AccessKeyEnv string `yaml:"access_key_env"` // nome da env var com access key
	SecretKeyEnv string `yaml:"secret_key_env"` // nome da env var com secret key
}

// BucketConfig define um destino de object storage pós-commit.
type BucketConfig struct {
	Name         string            `yaml:"name"`           // nome único dentro do storage
	Provider     string            `yaml:"provider"`       // s3 (expandível para gcs, azure)
	Endpoint     string            `yaml:"endpoint"`       // vazio = AWS default; preenchido = MinIO/compatível
	Region       string            `yaml:"region"`         // região AWS (default: us-east-1)
	Bucket       string            `yaml:"bucket"`         // nome do bucket
	Prefix       string            `yaml:"prefix"`         // prefixo de objetos no bucket (opcional)
	Mode         string            `yaml:"mode"`           // sync|offload|archive
	Retain       int               `yaml:"retain"`         // obrigatório para offload/archive; proibido para sync
	SyncStrategy string            `yaml:"sync_strategy"` // safe (default) | space_efficient — só para mode: sync
	AsyncUpload  bool              `yaml:"async_upload"`  // false (default) | true — libera lock/ACK antes do sync
	StallTimeout time.Duration     `yaml:"stall_timeout"` // inatividade máxima antes de cancelar upload (default: 5m)
	Credentials  BucketCredentials `yaml:"credentials"`   // credenciais via env vars
}

// StorageInfo contém configurações de armazenamento e rotação de um storage nomeado.
type StorageInfo struct {
	BaseDir                string         `yaml:"base_dir"`
	MaxBackups             int            `yaml:"max_backups"`
	AssemblerMode          string         `yaml:"assembler_mode"`              // eager|lazy (default: eager)
	AssemblerPendingMem    string         `yaml:"assembler_pending_mem_limit"` // ex: "8mb" (default: 8mb)
	AssemblerPendingMemRaw int64          `yaml:"-"`
	CompressionMode        string         `yaml:"compression_mode"`   // gzip|zst (default: gzip)
	ChunkShardLevels       int            `yaml:"chunk_shard_levels"` // 1|2 (default: 1, número de níveis de sharding de chunks)
	ChunkFsync             bool           `yaml:"chunk_fsync"`        // fsync nos writes de chunk staging (default: false)
	VerifyIntegrity        bool           `yaml:"verify_integrity"`   // valida integridade do archive antes do rotate (default: false)
	Buckets                []BucketConfig `yaml:"buckets"`            // destinos de object storage pós-commit (opcional)
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

		// Bucket configs (object storage pós-commit)
		if err := validateBuckets(name, s.Buckets); err != nil {
			return err
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

	// Gap Detection: deprecated in v3.0.0 — kept for YAML backward compat.
	// Ignored at runtime; WarnDeprecated() emits a log warning at startup.

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

// validateBuckets valida a configuração dos buckets de object storage de um storage.
func validateBuckets(storageName string, buckets []BucketConfig) error {
	if len(buckets) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(buckets))
	for i, b := range buckets {
		prefix := fmt.Sprintf("storages.%s.buckets[%d]", storageName, i)

		// Nome obrigatório e único
		if b.Name == "" {
			return fmt.Errorf("%s.name is required", prefix)
		}
		if _, dup := seen[b.Name]; dup {
			return fmt.Errorf("%s.name %q is duplicated", prefix, b.Name)
		}
		seen[b.Name] = struct{}{}

		// Provider
		b.Provider = strings.ToLower(strings.TrimSpace(b.Provider))
		if b.Provider == "" {
			b.Provider = "s3"
		}
		if b.Provider != "s3" {
			return fmt.Errorf("%s.provider must be s3, got %q", prefix, b.Provider)
		}

		// Region default
		if b.Region == "" {
			b.Region = "us-east-1"
		}

		// Bucket obrigatório
		if b.Bucket == "" {
			return fmt.Errorf("%s.bucket is required", prefix)
		}

		// Mode
		b.Mode = strings.ToLower(strings.TrimSpace(b.Mode))
		if b.Mode == "" {
			return fmt.Errorf("%s.mode is required (sync, offload, or archive)", prefix)
		}
		if b.Mode != BucketModeSync && b.Mode != BucketModeOffload && b.Mode != BucketModeArchive {
			return fmt.Errorf("%s.mode must be sync, offload, or archive, got %q", prefix, b.Mode)
		}

		// Retain: obrigatório para offload/archive, proibido para sync
		switch b.Mode {
		case BucketModeSync:
			if b.Retain != 0 {
				return fmt.Errorf("%s.retain must not be set for sync mode (sync mirrors local rotation)", prefix)
			}
		case BucketModeOffload, BucketModeArchive:
			if b.Retain < 1 {
				return fmt.Errorf("%s.retain must be > 0 for %s mode", prefix, b.Mode)
			}
		}

		// SyncStrategy: default safe, só válido para sync
		if b.SyncStrategy == "" {
			b.SyncStrategy = SyncStrategySafe
		}
		b.SyncStrategy = strings.ToLower(strings.TrimSpace(b.SyncStrategy))
		if b.Mode == BucketModeSync {
			if b.SyncStrategy != SyncStrategySafe && b.SyncStrategy != SyncStrategySpaceEfficient {
				return fmt.Errorf("%s.sync_strategy must be safe or space_efficient, got %q", prefix, b.SyncStrategy)
			}
		} else if b.SyncStrategy != SyncStrategySafe {
			return fmt.Errorf("%s.sync_strategy is only valid for sync mode, got mode %q", prefix, b.Mode)
		}

		// AsyncUpload: proibido para offload (sempre bloqueante)
		if b.AsyncUpload && b.Mode == BucketModeOffload {
			return fmt.Errorf("%s.async_upload must not be set for offload mode (offload is always blocking)", prefix)
		}

		// Credenciais obrigatórias
		if b.Credentials.AccessKeyEnv == "" {
			return fmt.Errorf("%s.credentials.access_key_env is required", prefix)
		}
		if b.Credentials.SecretKeyEnv == "" {
			return fmt.Errorf("%s.credentials.secret_key_env is required", prefix)
		}

		buckets[i] = b
	}
	return nil
}
