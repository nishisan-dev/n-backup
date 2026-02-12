# StreamGuard Agent — Especificação Técnica

## 1. Visão Geral

O **StreamGuard** é um sistema de backup client-server escrito em Go, projetado para realizar streaming de dados diretamente da origem para o destino **sem criar arquivos temporários no disco de origem**. O resultado é um arquivo `.tar.gz` padrão Linux, preservando caminhos, permissões e estrutura de diretórios.

### 1.1 Problema

Em servidores com pouco espaço em disco ou discos lentos, o método tradicional ("compactar primeiro, enviar depois") é inviável para grandes volumes. O StreamGuard elimina essa limitação ao fazer `tar | gzip | network` em um único pipeline de streaming.

### 1.2 Origem

Evolução do script shell abaixo para um binário Go com mTLS, resiliência e operação autônoma:

```bash
tar -cvf - -C / app/scripts/ home/ etc/ | gzip | ssh "$REMOTE_HOST" "cat > '$REMOTE_DIR/$TMP_FILE'"
```

---

## 2. Decisões de Arquitetura

### 2.1 Transporte: TCP Puro + mTLS

| Decisão | Justificativa |
|---|---|
| **TCP puro** em vez de HTTP/2 ou gRPC | Fluxo unidirecional (Client → Server) sem necessidade de multiplexação, content negotiation ou middleware HTTP. Zero overhead por byte. |
| **TLS 1.3 com Mutual Auth (mTLS)** | Autenticação mútua obrigatória. Sem SSH keys, sem shell remoto. |
| **Protocolo binário customizado** | Header mínimo (~60 bytes por sessão). O payload é o stream raw do tar+gzip. |

### 2.2 Formato de Saída

- **`.tar.gz` padrão** — compatível com `tar xzf` do Linux.
- O server **não descompacta** — grava o stream raw diretamente em disco.
- Preserva: caminhos relativos, permissões, ownership, symlinks.

### 2.3 Streaming Pipeline

```
fs.WalkDir ──▶ tar.Writer ──▶ gzip.Writer ──▶ RingBuffer ──▶ tls.Conn ──▶ Server (io.Copy → disk)
     │                                    │
     └── excludes/includes (glob)           └── backpressure (bloqueia se cheio)
```

- O RingBuffer implementa `io.Writer` e aplica backpressure quando cheio.
- Sender goroutine lê do buffer por offset absoluto e envia para a conexão.
- ACK reader processa SACKs do server e avança o tail do buffer.
- SHA-256 é calculado inline via `io.MultiWriter` sobre o stream compactado.
- Se a conexão cair, o sender reconecta e retoma do último offset válido.

### 2.4 Agent: Daemon Mode

O agent opera como **daemon** (processo de longa duração), com scheduler interno para execução periódica. Compatível com systemd.

### 2.5 Restore

**Fora do escopo da v1.** O arquivo `.tar.gz` gerado é extraível manualmente:

```bash
tar xzf backup.tar.gz -C /restore/path
```

---

## 3. Protocolo NBackup (TCP Binário)

### 3.1 Visão Geral de Sessão

```
Client                                     Server
  │                                          │
  │──── TLS 1.3 Handshake (mTLS) ──────────▶│
  │◀─── TLS Established ──────────────────── │
  │                                          │
  │──── HANDSHAKE (agent, storage, ver) ──▶ │
  │◀─── ACK (status, sessionID) ────────── │
  │                                          │
  │──── DATA STREAM (tar.gz bytes) ───────▶ │  ← bulk transfer
  │     ... streaming contínuo ...           │
  │◀─── SACK (offset confirmado) ────────── │  ← a cada 64MB
  │     ... mais dados + SACKs ...           │
  │──── EOF ─────────────────────────────▶ │
  │                                          │
  │──── TRAILER (SHA-256, size) ─────────▶ │
  │◀─── FINAL ACK (status) ───────────── │
  │                                          │
  │──── Conexão encerrada ──────────────▶ │

--- RESUME (após queda de conexão) ---

  │──── TLS 1.3 Handshake (mTLS) ──────────▶│
  │◀─── TLS Established ──────────────────── │
  │                                          │
  │──── RESUME (sessionID, agent, stor) ──▶ │
  │◀─── ResumeACK (status, lastOffset) ─── │
  │                                          │
  │──── DATA STREAM (do lastOffset) ──────▶ │
  │     ... fluxo normal continua ...        │
```

### 3.2 Frames

#### Handshake (Client → Server)

```
┌──────────┬──────┬──────────────────┬───────┬───────────────────┬───────┐
│ "NBKP"   │ Ver  │ AgentName (UTF8) │ '\n'  │ StorageName (UTF8) │ '\n'  │
│ 4 bytes  │ 1B   │ variável         │ 1B    │ variável           │ 1B    │
└──────────┴──────┴──────────────────┴───────┴───────────────────┴───────┘
```

- **Magic**: `0x4E 0x42 0x4B 0x50` ("NBKP")
- **Ver**: Versão do protocolo (`0x01`)
- **AgentName**: Identificador UTF-8 do agent, delimitado por `\n`
- **StorageName**: Nome do storage de destino no server, delimitado por `\n`

#### ACK (Server → Client)

```
┌──────────┬──────────────────┬───────┬────────────────┬───────┐
│ Status   │ Message (UTF8)   │ '\n'  │ SessionID (UTF8)│ '\n'  │
│ 1 byte   │ variável (opt)   │ 1B    │ variável (opt)  │ 1B    │
└──────────┴──────────────────┴───────┴────────────────┴───────┘
```

| Status | Código | Significado |
|---|---|---|
| GO | `0x00` | Pronto para receber |
| FULL | `0x01` | Disco cheio no destino |
| BUSY | `0x02` | Backup deste agent:storage já em andamento |
| REJECT | `0x03` | Agent não autorizado |
| STORAGE_NOT_FOUND | `0x04` | Storage nomeado não existe no server |

O campo `SessionID` é um UUID v4 gerado pelo server, usado para identificar a sessão em caso de resume.

#### Data Stream (Client → Server)

Bytes raw do pipeline `tar | gzip`. **Sem framing** — o stream é contínuo até o client fechar a escrita (half-close TCP).

#### Trailer (Client → Server)

```
┌──────────┬─────────────────────────┬───────────┐
│ "DONE"   │ SHA-256 (binary)        │ Size      │
│ 4 bytes  │ 32 bytes                │ 8B uint64 │
└──────────┴─────────────────────────┴───────┘
```

#### Final ACK (Server → Client)

```
┌──────────┐
│ Status   │
│ 1 byte   │
└──────────┘
```

| Status | Código | Significado |
|---|---|---|
| OK | `0x00` | Checksum válido, backup gravado, rotação feita |
| CHECKSUM_MISMATCH | `0x01` | Hash não confere, arquivo descartado |
| WRITE_ERROR | `0x02` | Erro de I/O no destino |

### 3.3 Health Check

Sessão independente (conexão separada):

```
Client → Server: "PING" (4 bytes)
Server → Client: Status (1B) + DiskFree (8B uint64) + '\n'
```

CLI: `nbackup-agent health <server:port>`

### 3.4 Resume Protocol

Quando uma conexão cai mid-stream, o agent pode reconectar e retomar de onde parou.

#### RESUME (Client → Server)

```
┌──────────┬──────┬────────────────┬───────┬──────────────────┬───────┬───────────────────┬───────┐
│ "RSME"   │ Ver  │ SessionID (UTF8)│ '\n'  │ AgentName (UTF8) │ '\n'  │ StorageName (UTF8) │ '\n'  │
│ 4 bytes  │ 1B   │ variável         │ 1B    │ variável          │ 1B    │ variável            │ 1B    │
└──────────┴──────┴────────────────┴───────┴──────────────────┴───────┴───────────────────┴───────┘
```

- **Magic**: `0x52 0x53 0x4D 0x45` ("RSME")
- **SessionID**: UUID da sessão original (retornado no ACK do Handshake)

#### ResumeACK (Server → Client)

```
┌──────────┬─────────────┐
│ Status   │ LastOffset    │
│ 1 byte   │ 8B uint64     │
└──────────┴─────────────┘
```

| Status | Código | Significado |
|---|---|---|
| OK | `0x00` | Resume aceito, continuar do LastOffset |
| NOT_FOUND | `0x01` | Sessão expirada ou inválida, reiniciar |

#### SACK (Server → Client)

```
┌──────────┬─────────────┐
│ "SACK"   │ Offset        │
│ 4 bytes  │ 8B uint64     │
└──────────┴─────────────┘
```

Enviado periodicamente pelo server (a cada 64MB) para confirmar recebimento. O agent avança o tail do ring buffer, liberando espaço para novas escritas.

### 3.5 Parallel Streaming

Para backups grandes, o agent pode usar **múltiplos streams paralelos** para aumentar o throughput.

#### Fluxo

```
Client                                     Server
  │                                          │
  │──── Handshake + ACK GO (normal) ───────▶│
  │──── ParallelInit (maxStreams, chunkSize)▶│  ← extensão após ACK
  │                                          │
  │──── DATA stream 0 (primário) ─────────▶ │  ← conexão principal
  │                                          │
  │──── ParallelJoin (sessionID, idx=1) ──▶ │  ← nova conexão TLS
  │◀─── ParallelACK (OK/FULL/NOT_FOUND) ─── │
  │──── DATA stream 1 ────────────────────▶ │
  │◀─── ChunkSACK (idx, seq, offset) ────── │  ← ACK por chunk/stream
  │                                          │
  │──── ParallelJoin (sessionID, idx=2) ──▶ │  ← mais streams...
  │                                          │
  │──── Trailer (SHA-256, size total) ────▶ │  ← via stream 0
  │◀─── FINAL ACK ─────────────────────── │
```

#### ParallelInit (Client → Server)

Enviado imediatamente após o ACK GO na conexão primária:

```
┌──────────┬───────────┐
│ MaxStreams│ ChunkSize   │
│ 1 byte   │ 4B uint32   │
└──────────┴───────────┘
```

- **MaxStreams**: Número máximo de streams (1-8)
- **ChunkSize**: Tamanho de cada chunk em bytes (default: 1MB)

#### ParallelJoin (Client → Server)

Enviado em uma **nova conexão TLS** para unir-se a uma sessão existente:

```
┌──────────┬────────────────┬───────┬────────────┐
│ "PJIN"   │ SessionID (UTF8)│ '\n'  │ StreamIndex │
│ 4 bytes  │ variável        │ 1B    │ 1 byte      │
└──────────┴────────────────┴───────┴────────────┘
```

#### ParallelACK (Server → Client)

```
┌──────────┐
│ Status   │
│ 1 byte   │
└──────────┘
```

| Status | Código | Significado |
|---|---|---|
| OK | `0x00` | Stream aceito |
| FULL | `0x01` | Sessão no limite de streams |
| NOT_FOUND | `0x02` | SessionID não encontrado |

#### ChunkSACK (Server → Client)

ACK seletivo por stream, enviado nos streams secundários:

```
┌──────────┬────────────┬──────────┬──────────┐
│ "CSAK"   │ StreamIndex │ ChunkSeq  │ Offset    │
│ 4 bytes  │ 1 byte      │ 4B uint32 │ 8B uint64 │
└──────────┴────────────┴──────────┴──────────┘
```

#### Configuração

```yaml
backups:
  - name: "app"
    storage: "scripts"
    parallels: 0         # 0 = single stream (padrão)

  - name: "home"
    storage: "home-dirs"
    parallels: 4         # 4 streams paralelos
```

- **parallels**: `0` desabilita (single stream), `1-8` define o máximo de streams.
- O agent usa um **Dispatcher** (round-robin) e um **AutoScaler** (histerese) para distribuir chunks entre streams.

---

## 4. Configuração

### 4.1 Agent (`agent.yaml`)

```yaml
agent:
  name: "web-server-01"

daemon:
  schedule: "0 2 * * *"        # Cron expression (diário às 02h)

server:
  address: "backup.nishisan.dev:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/agent.pem
  client_key: /etc/nbackup/agent-key.pem

backup:
  sources:
    - path: /app/scripts
    - path: /home
    - path: /etc
  exclude:
    - "*/access-logs/"
    - "*.log"
    - "*/tmp/sess*"
    - "node_modules/**"
    - ".git/**"

retry:
  max_attempts: 5
  initial_delay: 1s
  max_delay: 5m

logging:
  level: info                  # debug, info, warn, error
  format: json                 # json, text
```

### 4.2 Server (`server.yaml`)

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storage:
  base_dir: /var/backups/nbackup
  max_backups: 5               # Rotação por índice: manter N mais recentes

logging:
  level: info
  format: json
```

### 4.3 Rotação por Índice (Server)

O server organiza os backups por agent e mantém no máximo `max_backups`:

```
/var/backups/nbackup/
  └── web-server-01/
      ├── 2026-02-11T02:00:00.tar.gz    ← mais recente
      ├── 2026-02-10T02:00:00.tar.gz
      ├── 2026-02-09T02:00:00.tar.gz
      ├── 2026-02-08T02:00:00.tar.gz
      └── 2026-02-07T02:00:00.tar.gz    ← removido quando o próximo chegar (max=5)
```

---

## 5. Resiliência

### 5.1 Retry com Exponential Backoff

Aplica-se à **conexão inicial** e ao **health check**. Se a conexão cai **mid-stream**, o backup é abortado e reagendado.

```
Tentativa 1 → falha → aguarda 1s
Tentativa 2 → falha → aguarda 2s
Tentativa 3 → falha → aguarda 4s
Tentativa 4 → falha → aguarda 8s (capped em max_delay)
Tentativa 5 → falha → ABORT, log error
```

### 5.2 Lock de Execução

O daemon garante que **apenas um backup por agent** é executado simultaneamente (mutex interno). O server também rejeita conexões duplicadas do mesmo agent (`BUSY`).

### 5.3 Atomicidade de Escrita

O server grava em arquivo temporário (`.tmp`) e só renomeia (atomic rename) após validação do checksum SHA-256.

### 5.4 Resume de Backups

Quando a conexão cai mid-stream, o agent tenta reconectar e resumir automaticamente.

**Mecânica:**

1. O agent mantém um **ring buffer** em memória (256MB padrão, configurável até 1GB).
2. O server envia **SACKs** a cada 64MB confirmando recebimento.
3. Ao receber um SACK, o agent avança o tail do buffer, liberando espaço.
4. Se a conexão cair, o agent reconecta e envia **RESUME** com o `sessionID`.
5. O server responde com o último offset gravado em disco.
6. O agent retoma o envio a partir desse offset (se ainda estiver no buffer).

**Limites:**

- Máximo de 5 tentativas de resume com backoff exponencial (2s, 4s, 8s...).
- Se o offset não estiver mais no buffer, o backup reinicia do zero.
- Sessões parciais no server expiram após 1 hora (TTL).
- O `.tmp` parcial é deletado na expiração.

```yaml
resume:
  buffer_size: 256mb    # Tamanho do ring buffer (kb, mb, gb)
```

### 5.5 Graceful Shutdown

O daemon responde a `SIGTERM` e `SIGINT`:
- Se ocioso: shutdown imediato.
- Se backup em andamento: aguarda conclusão ou timeout configurável antes de abortar.

---

## 6. Estrutura do Projeto

```
n-backup/
├── cmd/
│   ├── nbackup-agent/           # Entrypoint do daemon (client)
│   │   └── main.go
│   └── nbackup-server/          # Entrypoint do server
│       └── main.go
├── internal/
│   ├── agent/                   # Scanner, streamer, scheduler
│   ├── server/                  # Receiver, storage, rotação
│   ├── protocol/                # Frames, handshake, parser
│   ├── pki/                     # Geração de certificados mTLS
│   ├── config/                  # Parsing YAML
│   └── logging/                 # Logger estruturado
├── docs/
│   ├── specification.md         # Este documento
│   └── diagrams/
│       ├── architecture.puml
│       └── protocol_sequence.puml
├── configs/
│   ├── agent.example.yaml
│   └── server.example.yaml
├── planning/
├── go.mod
├── go.sum
└── README.md
```

---

## 7. CLI

```bash
# Agent (daemon)
nbackup-agent --config /etc/nbackup/agent.yaml

# Agent (execução única, para testes)
nbackup-agent --config agent.yaml --once

# Health check
nbackup-agent health backup.nishisan.dev:9847

# Server
nbackup-server --config /etc/nbackup/server.yaml

# PKI (futuro, após v1)
nbackup-agent cert init-ca
nbackup-agent cert gen-host --name web-server-01
```

---

## 8. Stack Técnica

| Recurso | Tecnologia |
|---|---|
| Linguagem | Go (Golang) |
| Transporte | TCP puro sobre TLS 1.3 |
| Segurança | mTLS (Mutual TLS) |
| Compactação | gzip (`compress/gzip` stdlib) |
| Empacotamento | tar (`archive/tar` stdlib) |
| Configuração | YAML (`gopkg.in/yaml.v3`) |
| Logging | `slog` (stdlib Go 1.21+) |
| Scheduler | `robfig/cron` ou implementação interna |
| Distribuição | Binário estático (GoReleaser) |

---

## 9. Fora do Escopo (v1)

- Restore via CLI (extrair manualmente com `tar xzf`)
- Backup incremental / diferencial
- Deduplicação
- Interface web / API REST
- PKI integrada (certificados gerenciados externamente na v1)
- Compressão Zstd (gzip na v1 para compatibilidade)
