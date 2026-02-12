package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ServerConfig representa a configuração completa do nbackup-server.
type ServerConfig struct {
	Server   ServerListen           `yaml:"server"`
	TLS      TLSServer              `yaml:"tls"`
	Storages map[string]StorageInfo `yaml:"storages"`
	Logging  LoggingInfo            `yaml:"logging"`
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
	return nil
}
