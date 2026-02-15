// Copyright (c) 2025 Nishisan. All rights reserved.
// Use of this source code is governed by the N-Backup License (Non-Commercial Evaluation)
// that can be found in the LICENSE file.

package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ServerConfig representa a configuração completa do nbackup-server.
type ServerConfig struct {
	Server       ServerListen           `yaml:"server"`
	TLS          TLSServer              `yaml:"tls"`
	Storages     map[string]StorageInfo `yaml:"storages"`
	Logging      LoggingInfo            `yaml:"logging"`
	FlowRotation FlowRotationConfig     `yaml:"flow_rotation"`
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
	BaseDir    string `yaml:"base_dir"`
	MaxBackups int    `yaml:"max_backups"`
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
			c.Storages[name] = s
		}
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

	return nil
}
