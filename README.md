# n-backup

[![CI](https://github.com/nishisan-dev/n-backup/actions/workflows/ci.yml/badge.svg)](https://github.com/nishisan-dev/n-backup/actions/workflows/ci.yml)
[![Release](https://github.com/nishisan-dev/n-backup/actions/workflows/release.yml/badge.svg)](https://github.com/nishisan-dev/n-backup/actions/workflows/release.yml)
[![License: Proprietary](https://img.shields.io/badge/License-Proprietary-red.svg)](LICENSE)

Sistema de backup **high-performance** client-server escrito em Go. Streaming direto da origem para o destino — **sem arquivos temporários** — via TCP com **mTLS** (TLS 1.3).

---

## ✨ Features

| Feature | Descrição |
|---------|-----------|
| **Streaming Nativo** | Pipeline `Disk → Tar → Gzip → Network` via `io.Pipe`. Zero arquivos temporários na origem. |
| **Zero-Footprint** | Otimizado para baixo consumo de CPU e RAM. Binário estático, sem dependências. |
| **Segurança mTLS** | Autenticação mútua obrigatória via TLS 1.3. Sem SSH, sem shell remoto. |
| **Integridade SHA-256** | Hash calculado inline durante streaming. Validação dupla (agent + server). |
| **Resume Mid-Stream** | Ring buffer em memória (256MB) permite retomar backups interrompidos. |
| **Parallel Streaming** | Até 8 streams TLS paralelos com AutoScaler para maximizar throughput. |
| **Rotação Automática** | Server mantém os N backups mais recentes por agent/storage. |
| **Retry Exponential** | Reconexão automática com backoff exponencial configurável. |
| **Named Storages** | Múltiplos storages no server com políticas de rotação independentes. |
| **Progress Bar** | Visualização de progresso em backups manuais (MB/s, ETA, retries). |

---

## 🏗️ Arquitetura

![Arquitetura NBackup](https://uml.nishisan.dev/proxy?src=https://raw.githubusercontent.com/nishisan-dev/n-backup/refs/heads/main/docs/diagrams/architecture.puml)

### Componentes

| Componente | Descrição |
|-----------|-----------|
| **nbackup-agent** | Daemon que executa backups periodicamente (cron). Lê arquivos, compacta e envia via TCP+mTLS. |
| **nbackup-server** | Recebe streams, valida integridade (SHA-256), grava atomicamente e faz rotação automática. |

### Pipeline de Dados

```
fs.WalkDir → tar.Writer → gzip.Writer → RingBuffer → tls.Conn → Server (io.Copy → disco)
```

---

## 📦 Instalação

### Via pacote `.deb` (Ubuntu/Debian) — Recomendado

Baixe o pacote `.deb` da [página de Releases](https://github.com/nishisan-dev/n-backup/releases):

```bash
# amd64
wget https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-agent_<VERSION>_amd64.deb
wget https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-server_<VERSION>_amd64.deb

# arm64
wget https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-agent_<VERSION>_arm64.deb
wget https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-server_<VERSION>_arm64.deb

# Instalar
sudo dpkg -i nbackup-agent_<VERSION>_amd64.deb
sudo dpkg -i nbackup-server_<VERSION>_amd64.deb
```

O pacote `.deb` inclui:
- Binário em `/usr/bin/`
- Config exemplo em `/etc/nbackup/`
- Systemd unit com hardening de segurança
- Man page (`man nbackup-agent`, `man nbackup-server`)

### Via binário estático

```bash
# Download na página de Releases
wget https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-agent-linux-amd64
wget https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-server-linux-amd64

chmod +x nbackup-agent-linux-amd64 nbackup-server-linux-amd64
sudo mv nbackup-agent-linux-amd64 /usr/local/bin/nbackup-agent
sudo mv nbackup-server-linux-amd64 /usr/local/bin/nbackup-server
```

### Build from source

```bash
git clone https://github.com/nishisan-dev/n-backup.git
cd n-backup
go build -o bin/nbackup-agent ./cmd/nbackup-agent
go build -o bin/nbackup-server ./cmd/nbackup-server
```

---

## 🔐 Configuração de Certificados (mTLS)

O n-backup exige **mutual TLS** — agent e server precisam de certificados assinados pela mesma CA.

```bash
# 1. Criar CA
openssl ecparam -genkey -name prime256v1 -out ca-key.pem
openssl req -new -x509 -sha256 -key ca-key.pem -out ca.pem -days 3650 -subj "/CN=NBackup CA"

# 2. Criar cert do Server (ajuste os SANs)
openssl ecparam -genkey -name prime256v1 -out server-key.pem
openssl req -new -sha256 -key server-key.pem -out server.csr -subj "/CN=nbackup-server"
echo 'subjectAltName=DNS:backup.example.com,IP:127.0.0.1
extendedKeyUsage=serverAuth' > server-ext.cnf
openssl x509 -req -sha256 -in server.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out server.pem -days 365 -extfile server-ext.cnf

# 3. Criar cert do Agent
openssl ecparam -genkey -name prime256v1 -out agent-key.pem
openssl req -new -sha256 -key agent-key.pem -out agent.csr -subj "/CN=web-server-01"
echo 'extendedKeyUsage=clientAuth' > agent-ext.cnf
openssl x509 -req -sha256 -in agent.csr -CA ca.pem -CAkey ca-key.pem \
  -CAcreateserial -out agent.pem -days 365 -extfile agent-ext.cnf
```

> ⚠️ As chaves privadas devem ter permissão restrita: `chmod 600 *-key.pem`

Detalhes completos em [Guia de Instalação](docs/installation.md).

---

## ⚙️ Quick Start

### Server

```yaml
# /etc/nbackup/server.yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  scripts:
    base_dir: /var/backups/scripts
    max_backups: 5
  home-dirs:
    base_dir: /var/backups/home
    max_backups: 10

logging:
  level: info
  format: json
```

```bash
sudo systemctl start nbackup-server
```

### Agent

```yaml
# /etc/nbackup/agent.yaml
agent:
  name: "web-server-01"

daemon:
  schedule: "0 2 * * *"        # Diário às 02h

server:
  address: "backup.example.com:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/agent.pem
  client_key: /etc/nbackup/agent-key.pem

backups:
  - name: app
    storage: scripts
    sources:
      - path: /app/scripts
    exclude:
      - "*.log"

  - name: home
    storage: home-dirs
    parallels: 4
    sources:
      - path: /home
    exclude:
      - ".git/**"
      - "node_modules/**"

retry:
  max_attempts: 5
  initial_delay: 1s
  max_delay: 5m

logging:
  level: info
  format: json
```

```bash
sudo systemctl start nbackup-agent
```

---

## 🖥️ CLI

| Comando | Descrição |
|---------|-----------|
| `nbackup-agent --config agent.yaml` | Daemon mode — backups automáticos via cron |
| `nbackup-agent --config agent.yaml --once` | Executar backup uma vez e encerrar |
| `nbackup-agent --config agent.yaml --once --progress` | Backup manual com progress bar |
| `nbackup-agent health <addr> --config agent.yaml` | Health check do server |
| `nbackup-server --config server.yaml` | Iniciar server |

---

## 🔄 Restauração

Os backups são arquivos `.tar.gz` padrão Linux:

```bash
# Listar conteúdo
tar tzf backup.tar.gz

# Restaurar completo
tar xzf backup.tar.gz -C /restore/path

# Restaurar arquivo específico
tar xzf backup.tar.gz -C /restore/path home/user/file.txt
```

---

## 📚 Documentação

| Documento | Descrição |
|----------|-----------|
| [Arquitetura](docs/architecture.md) | C4 Model, componentes, fluxos, decisões técnicas |
| [Guia de Instalação](docs/installation.md) | Build, PKI/mTLS, configuração, systemd |
| [Guia de Uso](docs/usage.md) | Comandos, daemon, retry, rotação, troubleshooting |
| [Especificação Técnica](docs/specification.md) | Protocolo binário, frames, sessão, resume |
| [Diagramas](docs/diagrams/) | PlantUML: arquitetura, protocolo, C4, fluxo de dados |

---

## 📦 Distribuição

Cada release inclui:

| Artefato | Descrição |
|---------|-----------|
| `nbackup-agent-linux-amd64` | Binário estático (amd64) |
| `nbackup-agent-linux-arm64` | Binário estático (arm64) |
| `nbackup-server-linux-amd64` | Binário estático (amd64) |
| `nbackup-server-linux-arm64` | Binário estático (arm64) |
| `nbackup-agent_<ver>_amd64.deb` | Pacote Debian (amd64) — inclui systemd + man page |
| `nbackup-agent_<ver>_arm64.deb` | Pacote Debian (arm64) — inclui systemd + man page |
| `nbackup-server_<ver>_amd64.deb` | Pacote Debian (amd64) — inclui systemd + man page |
| `nbackup-server_<ver>_arm64.deb` | Pacote Debian (arm64) — inclui systemd + man page |
| `checksums.txt` | SHA-256 de todos os artefatos |

---

## 🛠️ Stack

| Recurso | Tecnologia |
|---------|-----------|
| Linguagem | Go |
| Transporte | TCP + TLS 1.3 (mTLS) |
| Compactação | gzip (stdlib) |
| Empacotamento | tar (stdlib) |
| Configuração | YAML |
| Logging | slog (JSON) |
| Distribuição | Binário estático + .deb |

---

## 📄 Licença

Proprietário — [N-Backup License (Non-Commercial Evaluation)](LICENSE).
