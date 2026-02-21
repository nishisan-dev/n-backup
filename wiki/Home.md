# n-backup

[![CI](https://github.com/nishisan-dev/n-backup/actions/workflows/ci.yml/badge.svg)](https://github.com/nishisan-dev/n-backup/actions/workflows/ci.yml)
[![Release](https://github.com/nishisan-dev/n-backup/actions/workflows/release.yml/badge.svg)](https://github.com/nishisan-dev/n-backup/actions/workflows/release.yml)
[![License: Proprietary](https://img.shields.io/badge/License-Proprietary-red.svg)](https://github.com/nishisan-dev/n-backup/blob/main/LICENSE)

Sistema de backup **high-performance** client-server escrito em Go. Streaming direto da origem para o destino — **sem arquivos temporários** — via TCP com **mTLS** (TLS 1.3).

---

## ✨ Features

| Feature | Descrição |
|---------|-------|
| **Streaming Nativo** | Pipeline `Disk → Tar → Gzip → Network` via `io.Pipe`. Zero arquivos temporários na origem. |
| **Zero-Footprint** | Otimizado para baixo consumo de CPU e RAM. Binário estático, sem dependências. |
| **Segurança mTLS** | Autenticação mútua obrigatória via TLS 1.3. Sem SSH, sem shell remoto. |
| **Integridade SHA-256** | Hash calculado inline durante streaming. Validação dupla (agent + server). |
| **Resume Mid-Stream** | Ring buffer em memória (configurável, padrão 256MB) permite retomar backups interrompidos. |
| **Parallel Streaming** | Até 255 streams TLS paralelos com chunk-based dispatch para maximizar throughput. |
| **Compressão Paralela** | `pgzip` (klauspost) com goroutines paralelas — até 3x mais rápido que gzip stdlib. |
| **Control Channel** | Conexão TLS persistente para keep-alive (PING/PONG), medição de RTT e orquestração server-side. |
| **Graceful Flow Rotation** | Server solicita drenagem de streams via ControlRotate — zero data loss em reconexões. |
| **RTT Metrics** | RTT EWMA contínuo via control channel, com status do server (carga, disco). |
| **Rotação Automática** | Server mantém os N backups mais recentes por agent/storage. Rotações emitem evento e log. |
| **Retry Exponencial** | Reconexão automática com backoff exponencial configurável. Contador resetado após reconexão bem-sucedida. |
| **Named Storages** | Múltiplos storages no server com políticas de rotação independentes. |
| **Bandwidth Throttling** | Limite de upload configurável por backup entry (Token Bucket). |
| **AutoScaler** | Escala dinâmica de streams com modos `efficiency` e `adaptive`. |
| **Chunk Sharding** | Staging de chunks em 1 ou 2 níveis de subdiretórios (`chunk_shard_levels`). Reduz pressão no filesystem em sessões paralelas intensas. |
| **WebUI** | SPA embarcada de observabilidade com sessões, sparklines, events e gauges. Dados persistidos em JSONL (sobrevivem a reinicios). |
| **Schedule por Backup** | Cada backup entry possui sua própria cron expression. |
| **Hot Reload (SIGHUP)** | Recarrega configuração sem downtime via `systemctl reload`. |

---

## 🏗️ Arquitetura

![Arquitetura NBackup](https://uml.nishisan.dev/proxy?src=https://raw.githubusercontent.com/nishisan-dev/n-backup/refs/heads/main/docs/diagrams/architecture.puml)

### Componentes

| Componente | Descrição |
|-----------|-----------|
| **nbackup-agent** | Daemon com scheduler independente por backup entry. Lê arquivos, compacta e envia via TCP+mTLS. Suporta hot reload (SIGHUP). |
| **nbackup-server** | Recebe streams (single ou paralelo), valida integridade (SHA-256), grava atomicamente e faz rotação automática. Expira sessões inativas (idle-based). |

### Pipeline de Dados

**Single Stream:**
```
fs.WalkDir → tar.Writer → pgzip.Writer → RingBuffer → tls.Conn → Server (io.Copy → disco)
```

**Parallel Streaming:**
```
fs.WalkDir → tar.Writer → pgzip.Writer → Dispatcher (round-robin) → N × tls.Conn → Server (ChunkAssembler → disco)
```

**Control Channel (persistente):**
```
Agent ←→ Server: CTRL magic → ControlPing/Pong (keep-alive + RTT) + ControlRotate/RotateACK (orquestração)
```

---

## ⬇️ Quick Download — Latest Release

| Artefato | amd64 | arm64 |
|----------|:-----:|:-----:|
| **Agent (.deb)** | [⬇ deb](https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-agent_amd64.deb) | [⬇ deb](https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-agent_arm64.deb) |
| **Server (.deb)** | [⬇ deb](https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-server_amd64.deb) | [⬇ deb](https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-server_arm64.deb) |
| **Agent (binário)** | [⬇ bin](https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-agent-linux-amd64) | [⬇ bin](https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-agent-linux-arm64) |
| **Server (binário)** | [⬇ bin](https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-server-linux-amd64) | [⬇ bin](https://github.com/nishisan-dev/n-backup/releases/latest/download/nbackup-server-linux-arm64) |
| **Checksums** | [checksums.txt](https://github.com/nishisan-dev/n-backup/releases/latest/download/checksums.txt) | |

---

## 📚 Documentação Completa

| Página | Descrição |
|--------|-----------|
| [[Arquitetura]] | C4 Model, componentes, fluxos, decisões técnicas |
| [[Instalação]] | Build, PKI/mTLS, configuração, systemd |
| [[Guia de Uso\|Guia-de-Uso]] | Comandos, daemon, retry, rotação, troubleshooting |
| [[Especificação Técnica\|Especificação-Técnica]] | Protocolo binário, frames, sessão, resume, parallel streaming |
| [[Configuração de Exemplo\|Configuração-de-Exemplo]] | Exemplos completos de `agent.yaml` e `server.yaml` |
| [[WebUI]] | Painel de observabilidade (sessões, events, gauges) |
| [[FAQ]] | Troubleshooting e perguntas frequentes |

---

## 🛠️ Stack

| Recurso | Tecnologia |
|---------|-----------|
| Linguagem | Go |
| Transporte | TCP + TLS 1.3 (mTLS) |
| Compactação | pgzip (klauspost/pgzip — compressão paralela multi-core) |
| Empacotamento | tar (stdlib) |
| Configuração | YAML |
| Logging | slog (JSON/text) |
| Distribuição | Binário estático + .deb |

---

## 📄 Licença

Proprietário — [N-Backup License (Non-Commercial Evaluation)](https://github.com/nishisan-dev/n-backup/blob/main/LICENSE).
