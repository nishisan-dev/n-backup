# Arquitetura — n-backup

## 1. Visão de Contexto (C4 — Nível 1)

O **n-backup** é um sistema de backup client-server de alta performance escrito em Go. Ele opera como um par Agent/Server que transfere dados via streaming direto (sem arquivos temporários na origem) sobre TCP com mTLS.

```
┌──────────────────────────────────────────────────────────────────────────┐
│                           Infraestrutura do Cliente                     │
│                                                                         │
│  ┌─────────────────┐     Lê arquivos        ┌──────────────────┐        │
│  │  Filesystem     │ ◄──────────────────── │  nbackup-agent   │        │
│  │  (origem)       │     (fs.WalkDir)       │  (Daemon)        │        │
│  └─────────────────┘                        └────────┬─────────┘        │
│                                                      │                  │
└──────────────────────────────────────────────────────┼──────────────────┘
                                                       │
                                              TCP + TLS 1.3 (mTLS)
                                              Protocolo NBKP binário
                                                       │
┌──────────────────────────────────────────────────────┼──────────────────┐
│                           Infraestrutura do Server   │                  │
│                                                      ▼                  │
│                                              ┌───────────────┐          │
│  ┌─────────────────┐     Grava backup       │ nbackup-server │         │
│  │  Filesystem     │ ◄──────────────────── │  (Receiver)    │          │
│  │  (destino)      │     (atomic write)     └───────────────┘          │
│  └─────────────────┘                                                    │
│                                                                         │
└──────────────────────────────────────────────────────────────────────────┘
```

### Atores

| Ator | Descrição |
|------|-----------|
| **Administrador** | Configura agent e server via YAML, gera certificados mTLS, monitora via logs |
| **Cron/Scheduler** | Dispara backups automaticamente conforme expressão cron configurada |
| **Filesystem origem** | Diretórios/arquivos a serem incluídos no backup |
| **Filesystem destino** | Armazenamento de backups com rotação automática |

---

## 2. Visão de Container (C4 — Nível 2)

![Diagrama C4 Container](https://uml.nishisan.dev/proxy?src=https://raw.githubusercontent.com/nishisan-dev/n-backup/refs/heads/main/docs/diagrams/c4_container.puml)

### Containers

| Container | Tecnologia | Responsabilidade |
|-----------|-----------|-----------------|
| **nbackup-agent** | Go binary, daemon | Escaneia filesystem, compacta (tar+gzip), envia stream via mTLS, gerencia retry/resume |
| **nbackup-server** | Go binary, listener | Aceita conexões mTLS, valida integridade (SHA-256), grava atomicamente, faz rotação |
| **Protocolo NBKP** | TCP binário customizado | Handshake, data stream, SACK, resume, parallel streaming — ~60 bytes de overhead por sessão |

### Comunicação

- **Transporte**: TCP puro sobre TLS 1.3
- **Autenticação**: mTLS obrigatório — agent e server precisam de certificados assinados pela mesma CA
- **Formato de payload**: stream raw de `tar.gz` (sem framing no body)
- **Porta padrão**: `9847/tcp`

---

## 3. Visão de Componentes (C4 — Nível 3)

### 3.1. nbackup-agent

```
┌─────────────────────────────────────────────────────────────────┐
│                        nbackup-agent                            │
│                                                                 │
│  ┌───────────┐  ┌──────────┐  ┌──────────┐  ┌──────────────┐   │
│  │ Scheduler │─▶│ Scanner  │─▶│ Streamer │─▶│ RingBuffer   │   │
│  │ (cron)    │  │(WalkDir) │  │(tar+pgz)│  │(backpressure)│   │
│  └───────────┘  └──────────┘  └──────────┘  └───────┬──────┘   │
│                                                     │           │
│  ┌───────────┐  ┌──────────┐  ┌──────────┐  ┌───────▼──────┐   │
│  │ Config    │  │ Logger   │  │  Retry   │  │  TLS/Proto   │   │
│  │ (YAML)   │  │ (slog)   │  │(backoff) │  │  (mTLS)      │   │
│  └───────────┘  └──────────┘  └──────────┘  └──────────────┘   │
│                                                                 │
│  ┌───────────┐  ┌──────────────┐  ┌──────────────────────┐     │
│  │ Progress  │  │ Dispatcher   │  │ AutoScaler           │     │
│  │ (--once)  │  │ (round-robin)│  │ (hysteresis scaling) │     │
│  └───────────┘  └──────────────┘  └──────────────────────┘     │
│                                                                 │
│  ┌──────────────────────┐                                       │
│  │ ControlChannel       │                                       │
│  │ (keep-alive + RTT +  │                                       │
│  │  flow rotation ctrl) │                                       │
│  └──────────────────────┘                                       │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

| Componente | Arquivo | Responsabilidade |
|-----------|---------|-----------------|
| **Scheduler** | `internal/agent/scheduler.go` | Agenda execuções via cron expression (`robfig/cron`), timeout de 24h por job |
| **Daemon** | `internal/agent/daemon.go` | Loop principal, graceful shutdown (`SIGTERM`/`SIGINT`), hot-reload via `SIGHUP` |
| **Scanner** | `internal/agent/scanner.go` | `fs.WalkDir` com glob include/exclude, gera lista de arquivos para tar |
| **Streamer** | `internal/agent/streamer.go` | Pipeline `tar.Writer → pgzip.Writer → io.Pipe`, calcula SHA-256 inline |
| **RingBuffer** | `internal/agent/ringbuffer.go` | Buffer circular em memória (default 256MB), backpressure, suporte a resume |
| **Backup** | `internal/agent/backup.go` | Orquestrador: conecta, handshake, decide single/parallel, conn primária control-only (parallel) |
| **Dispatcher** | `internal/agent/dispatcher.go` | Round-robin de chunks, retry/reconnect por stream com backoff, dead stream marking |
| **AutoScaler** | `internal/agent/autoscaler.go` | Escala streams dinamicamente com histerese baseada em eficiência |
| **Progress** | `internal/agent/progress.go` | Barra de progresso para modo `--once --progress` (MB/s, ETA, retries) |
| **ControlChannel** | `internal/agent/control_channel.go` | Conexão TLS persistente com keep-alive (PING/PONG), RTT EWMA, recepção de ControlRotate para drenagem graceful de streams |

### 3.2. nbackup-server

```
┌──────────────────────────────────────────────────────────────┐
│                       nbackup-server                         │
│                                                              │
│  ┌───────────┐  ┌──────────────┐  ┌───────────────────┐     │
│  │ TLS       │─▶│ Handler      │─▶│ Storage Writer    │     │
│  │ Listener  │  │ (Protocol)   │  │ (.tmp → rename)   │     │
│  └───────────┘  └──────────────┘  └─────────┬─────────┘     │
│                                              │               │
│  ┌───────────┐  ┌──────────────┐  ┌─────────▼─────────┐     │
│  │ Config    │  │ Logger       │  │ Rotation Manager  │     │
│  │ (YAML)   │  │ (slog)       │  │ (max_backups)     │     │
│  └───────────┘  └──────────────┘  └───────────────────┘     │
│                                                              │
│  ┌────────────────────┐                                      │
│  │ ChunkAssembler     │                                      │
│  │ (parallel streams) │                                      │
│  └────────────────────┘                                      │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

| Componente | Arquivo | Responsabilidade |
|-----------|---------|-----------------|
| **Server** | `internal/server/server.go` | Listener TLS, aceita conexões, despacha para Handler |
| **Handler** | `internal/server/handler.go` | Protocolo: handshake, resume, health check, data stream, trailer, final ACK. Emite eventos (início/fim de sessão, rotações, reconexões) para a WebUI |
| **Storage** | `internal/server/storage.go` | Escrita atômica (`.tmp` → rename), rotação por `max_backups`, organização por agent. Rotação emite log e evento com lista de backups removidos |
| **Assembler** | `internal/server/assembler.go` | Reassembla chunks de streams paralelos na ordem correta via `GlobalSeq`. Staging de chunks suporta 1 ou 2 níveis de sharding (`chunk_shard_levels`) para reduzir entradas por diretório |
| **Slot** | `internal/server/slot.go` | Struct tipada por slot paralelo: estado (`Idle`, `Receiving`, `Disconnected`, `Disabled`), offsets, métricas de chunks (recebidos/perdidos/retransmitidos) e flow rotation. Substitui os `sync.Map` anteriores |

### 3.3. Módulos Compartilhados

| Módulo | Pacote | Responsabilidade |
|--------|--------|-----------------|
| **Config** | `internal/config/` | Parsing YAML, validação, defaults, `ParseByteSize`, `ControlChannelConfig` |
| **Protocol** | `internal/protocol/` | Frames binários (Handshake, ACK, SACK, Resume, Parallel, Control) |
| **PKI** | `internal/pki/` | Configuração TLS client/server, carregamento de certificados |
| **Logging** | `internal/logging/` | Factory de `slog.Logger` (JSON/text, nível configurável) |

---

## 4. Fluxo de Dados

![Fluxo de Dados](https://uml.nishisan.dev/proxy?src=https://raw.githubusercontent.com/nishisan-dev/n-backup/refs/heads/main/docs/diagrams/data_flow.puml)

### Pipeline de Streaming (Single Stream)

```
fs.WalkDir ──▶ tar.Writer ──▶ pgzip.Writer ──▶ RingBuffer ──▶ tls.Conn ──▶ Server (io.Copy → disk)
     │                                    │
     └── excludes/includes (glob)          └── backpressure (bloqueia se cheio)
```

1. **Scanner** percorre o filesystem respeitando regras de include/exclude
2. **tar.Writer** empacota arquivos preservando caminhos relativos, permissões e ownership
3. **gzip.Writer** comprime o stream inline
4. **SHA-256** é calculado via `io.TeeReader` sobre o stream compactado
5. **RingBuffer** aplica backpressure — bloqueia o producer se cheio (256MB default)
6. **Sender goroutine** lê do buffer por offset absoluto e envia para a conexão TLS
7. **ACK reader** processa SACKs do server e avança o tail do buffer

> **Nota:** A compressão usa `pgzip` (klauspost) com goroutines paralelas, obtendo throughput até 3x superior ao `compress/gzip` da stdlib.

### Pipeline Paralelo (v1.2.3+)

![Sessão Paralela](https://uml.nishisan.dev/proxy?src=https://raw.githubusercontent.com/nishisan-dev/n-backup/refs/heads/main/docs/diagrams/parallel_sequence.puml)

Quando `parallels > 0`:

1. Agent completa handshake normal na conexão primária
2. Envia `ParallelInit` com `maxStreams` e `chunkSize`
3. Conexão primária torna-se **control-only** (Trailer + FinalACK)
4. **Todos** os N streams (incluindo stream 0) conectam via `ParallelJoin` em conns TLS separadas
5. Server responde com `ParallelACK(status, lastOffset)` — `lastOffset=0` para first-join, `>0` para re-join
6. **Dispatcher** distribui chunks round-robin entre streams ativos, com `ChunkHeader` framing
7. Se um stream cai: retry com backoff exponencial (1s, 2s, 4s), re-join com resume do `lastOffset`
8. Stream permanentemente morto após 3 falhas — backup continua nos restantes
9. **ChunkAssembler** no server reassembla na ordem correta
10. Trailer e Final ACK trafegam pela conexão primária (control-only)

---

## 5. Protocolo NBKP

O protocolo binário customizado é otimizado para streaming unidirecional com overhead mínimo.

### Sessão Normal

```
Agent                                      Server
  │──── TLS 1.3 Handshake (mTLS) ────────▶│
  │──── HANDSHAKE (NBKP, agent, storage) ▶│
  │◀─── ACK (status, sessionID) ──────────│
  │──── DATA STREAM (tar.gz raw) ────────▶│
  │◀─── SACK (offset, a cada 64MB) ──────│
  │──── TRAILER (SHA-256, size) ──────────▶│
  │◀─── FINAL ACK (status) ──────────────│
```

### Frames

| Frame | Magic | Direção | Tamanho |
|-------|-------|---------|---------|
| Handshake | `NBKP` | C→S | ~60 bytes |
| ACK | — | S→C | variável |
| Data Stream | — | C→S | contínuo |
| SACK | `SACK` | S→C | 12 bytes |
| Trailer | `DONE` | C→S | 44 bytes |
| Final ACK | — | S→C | 1 byte |
| Resume | `RSME` | C→S | variável |
| ResumeACK | — | S→C | 9 bytes |
| ParallelInit | — | C→S | 5 bytes |
| ParallelJoin | `PJIN` | C→S | variável |
| ParallelACK | — | S→C | 9 bytes |
| ChunkHeader (v5) | — | C→S | 9 bytes |
| ChunkSACK | `CSAK` | S→C | 17 bytes |
| Health (PING) | `PING` | C→S | 4 bytes |
| Health (PONG) | — | S→C | 10 bytes |
| ControlPing | `CPNG` | C→S | 12 bytes |
| ControlPong | `CPNG` | S→C | 20 bytes |
| ControlRotate | `CROT` | S→C | 5 bytes |
| ControlRotateACK | `CRAK` | C→S | 5 bytes |
| ControlAdmit | `CADM` | S→C | 5 bytes |
| ControlDefer | `CDFE` | S→C | 8 bytes |
| ControlAbort | `CABT` | S→C | 8 bytes |
| ControlSlotPark | `CSLP` | C→S | 5 bytes |
| ControlSlotResume | `CSLR` | C→S | 5 bytes |

Para detalhes completos dos frames, veja a [[Especificação Técnica|Especificacao-Tecnica]].

---

## 6. Segurança

### mTLS (Mutual TLS)

- **TLS 1.3** obrigatório — não aceita versões anteriores
- **Autenticação mútua**: server valida certificado do agent, agent valida certificado do server
- **CA compartilhada**: ambos devem ter certificados assinados pela mesma Certificate Authority
- **Validação de identidade**: o server verifica que o **CN do certificado do agent corresponde ao `agentName`** informado no protocolo de handshake — rejeita a conexão se divergem
- **ECC P-256**: curva recomendada para chaves (`prime256v1`)
- **Sem SSH**: o sistema não depende de SSH ou shell remoto

### Hardening do Handshake

- **Path traversal**: nomes de agent, storage e backup são sanitizados contra `..`, `/`, `\`, null bytes e nomes ocultos (`.`)
- **Limite de campo**: campos do handshake limitados a 512 bytes (previne OOM/DoS)
- **Read deadline**: timeout de 10s para leitura do handshake (previne slowloris)

### Integridade de Dados

- **SHA-256 inline**: calculado durante o streaming via `io.TeeReader`/`io.MultiWriter` — sem releitura
- **Validação dupla**: agent calcula o hash, server recalcula independentemente e compara
- **Escrita atômica**: dados gravados em `.tmp`, renomeados apenas após validação do checksum
- **Descarte automático**: arquivo `.tmp` removido se checksum falhar

### Hardening (systemd)

As units systemd do pacote `.deb` incluem:
- `NoNewPrivileges=yes` — sem escalada de privilégios
- `PrivateTmp=yes` — `/tmp` isolado por serviço
- `ProtectKernelModules=yes`, `ProtectKernelTunables=yes` — kernel hardening
- `MemoryDenyWriteExecute=yes` — previne JIT malicioso
- `CPUSchedulingPriority=10`, `IOSchedulingClass=realtime` — prioridade defensiva

---

## 7. Resiliência

### Retry com Exponential Backoff

```
Tentativa 1 → falha → aguarda initial_delay (1s)
Tentativa 2 → falha → aguarda 2s
Tentativa 3 → falha → aguarda 4s
...
Tentativa N → falha → aguarda min(2^N × initial_delay, max_delay)
```

**Streams paralelos (v1.2.3+):** cada stream tem retry independente (até `maxRetriesPerStream=5` tentativas). O backup só falha quando todos os streams morrem (`ErrAllStreamsDead`). Conexões TCP usam **write deadline** para detectar half-open connections.

> **Fix (v2.6.0):** O contador de retries é **resetado para zero** após cada reconexão bem-sucedida — evitando a morte prematura de streams que enfrentam falhas intermitentes esporádicas.

### Resume de Sessão

#### Single Stream

1. Agent mantém **ring buffer** em memória (256MB default, até 1GB)
2. Server envia **SACK** a cada 1MB
3. Se conexão cair: agent reconecta, envia `RESUME` com `sessionID`
4. Server responde com último offset gravado
5. Agent retoma do offset (se ainda no buffer)
6. Máximo 5 tentativas de resume; sessão expira após 1h no server

#### Parallel Streams (v1.2.3+)

1. Server rastreia offsets por slot via struct `Slot` tipada (pré-alocada com `PreallocateSlots`)
2. Se stream cai: agent faz `ParallelJoin` novamente com mesmo `StreamIndex`
3. Server responde `ParallelACK(OK, lastOffset=N)` — resume do offset
4. Até 3 tentativas por stream; stream morto após esgotar
5. Cada slot possui estado (`Idle`, `Receiving`, `Disconnected`, `Disabled`) e métricas atômicas

### Job Timeout (v1.2.3+)

O scheduler configura `context.WithTimeout(24h)` por job, prevenindo zombie jobs.

### Graceful Shutdown

- Agent responde a `SIGTERM`/`SIGINT`/`SIGHUP` (reload)
- Se ocioso: shutdown imediato
- Se backup em andamento: aguarda conclusão antes de encerrar

### Control Channel (v1.3.8+)

O agent mantém uma conexão TLS persistente com o server (magic `CTRL`) para:

1. **Keep-alive**: PINGs periódicos configuráveis (`keepalive_interval`, default 30s)
2. **RTT EWMA**: Medição contínua de latência (Exponentially Weighted Moving Average)
3. **Status do server**: Carga (CPU) e espaço livre em disco no ControlPong
4. **Slot management**: Agent envia `ControlSlotPark(slotID)` para scale-down e `ControlSlotResume(slotID)` para scale-up de slots individuais
5. **Graceful Flow Rotation**: Server envia `ControlRotate(streamIndex)` → Agent drena o stream e responde `ControlRotateACK` — zero data loss
6. **Orquestração futura**: Frames `ControlAdmit`, `ControlDefer`, `ControlAbort` já definidos no protocolo

O canal reconecta automaticamente com exponential backoff (`reconnect_delay` até `max_reconnect_delay`).

```yaml
daemon:
  control_channel:
    enabled: true
    keepalive_interval: 30s
    reconnect_delay: 5s
    max_reconnect_delay: 5m
```

---

## 8. Observabilidade (WebUI — v2.0.0+)

O nbackup-server embarca uma **SPA de observabilidade** acessível via HTTP, servida a partir de assets estáticos incorporados no binário (`go:embed`). Um listener HTTP dedicado, protegido por ACL baseada em IP/CIDR, expõe endpoints REST e a interface web.

### APIs REST

| Endpoint | Descrição |
|----------|-----------|
| `GET /api/v1/health` | Status do server |
| `GET /api/v1/metrics` | Bytes recebidos, sessões |
| `GET /api/v1/sessions` | Sessões ativas |
| `GET /api/v1/sessions/{id}` | Detalhe de sessão (streams, sparklines, assembler) |
| `GET /api/v1/sessions/history` | Histórico de sessões finalizadas (ring buffer + JSONL) |
| `GET /api/v1/sessions/active-history` | Snapshots periódicos de sessões ativas (JSONL) |
| `GET /api/v1/agents` | Agentes conectados com stats (CPU, RAM, Disco) |
| `GET /api/v1/storages` | Storages com uso de disco (usado/total/percentual) |
| `GET /api/v1/events` | Eventos recentes (ring buffer + persistência JSONL) |
| `GET /api/v1/config/effective` | Configuração efetiva do server |

### WebUI (SPA)

- **Vanilla JS** + CSS (sem framework), embarcado via `go:embed`
- **Polling adaptativo**: atualiza dados a cada 2s (ativo) e views sob demanda
- **Views**: Overview, Sessions, Events, Config
- **Session Detail**: sparklines de throughput (Canvas), streams com uptime/reconnects, assembler progress
- **Session History**: tabela com badges coloridos por resultado (ok/checksum/write_error/timeout)
- **Connected Agents**: tabela com stats em gauges visuais (CPU/RAM/Disk)
- **Storages**: gauges de uso de disco com thresholds visuais (verde/amarelo/vermelho)

### Componentes

| Componente | Arquivo | Responsabilidade |
|-----------|---------|------------------|
| **Observability HTTP** | `internal/server/observability/http.go` | Router, handlers REST, ACL |
| **DTOs** | `internal/server/observability/dto.go` | Structs para serialização JSON |
| **Event Store** | `internal/server/observability/event_store.go` | Persistência JSONL com rotação |
| **Session History Store** | `internal/server/observability/session_history_store.go` | Ring + persistência JSONL de sessões finalizadas |
| **Active Session Store** | `internal/server/observability/active_session_store.go` | Snapshots periódicos de sessões ativas (ring + JSONL) |
| **WebUI Assets** | `internal/server/observability/web/` | SPA (HTML, CSS, JS) embarcados |

> **ACL obrigatória:** `web_ui.allow_origins` deve ser configurado quando `enabled: true`. Veja [[Configuração de Exemplo|Configuracao-de-Exemplo]].

---

## 9. Estrutura do Projeto

```
n-backup/
├── cmd/
│   ├── nbackup-agent/main.go        # Entrypoint do daemon (client)
│   └── nbackup-server/main.go       # Entrypoint do server
├── internal/
│   ├── agent/                        # Scanner, streamer, scheduler, ringbuffer
│   │   ├── autoscaler.go            #   AutoScaler de streams paralelos
│   │   ├── backup.go                #   Orquestrador de backup
│   │   ├── control_channel.go       #   Canal de controle persistente
│   │   ├── daemon.go                #   Daemon loop com graceful shutdown
│   │   ├── dispatcher.go            #   Round-robin de chunks + retry com reset
│   │   ├── dscp.go                  #   DSCP marking em sockets TCP
│   │   ├── monitor.go               #   Monitor de recursos (CPU, memória, disco)
│   │   ├── progress.go              #   Progress bar (--once)
│   │   ├── ringbuffer.go            #   Ring buffer para resume
│   │   ├── scanner.go               #   fs.WalkDir com glob
│   │   ├── scheduler.go             #   Cron scheduler wrapper
│   │   ├── stats_reporter.go        #   Reporter de stats para o server (control channel)
│   │   ├── streamer.go              #   Pipeline tar → pgzip → rede
│   │   └── throttle.go              #   ThrottledWriter (Token Bucket)
│   ├── config/                       # Parsing YAML + validação
│   ├── integration/                  # Testes de integração
│   ├── logging/                      # Factory de slog.Logger
│   ├── pki/                          # Configuração TLS client/server
│   ├── protocol/                     # Frames binários, reader, writer
│   └── server/                       # Receiver, handler, storage, assembler
│       ├── slot.go                  #   Slot struct (estado tipado, métricas atômicas, flow rotation)
│       └── observability/           #   WebUI + APIs REST + persistência JSONL
├── configs/                          # Exemplos de configuração
├── docs/                             # Documentação + diagramas PlantUML
├── packaging/                        # Empacotamento .deb
├── planning/                         # Artefatos de planejamento
├── wiki/                             # Conteúdo da GitHub Wiki
├── go.mod
├── go.sum
├── LICENSE
└── README.md
```

---

## 10. Decisões Arquiteturais

| # | Decisão | Alternativas | Justificativa |
|---|---------|-------------|---------------|
| 1 | **TCP puro + mTLS** | HTTP/2, gRPC, SSH pipe | Fluxo unidirecional sem necessidade de multiplexação HTTP. Zero overhead por byte transferido. |
| 2 | **Protocolo binário customizado** | Protocol Buffers, JSON-RPC | Header mínimo (~60 bytes/sessão). O payload é stream raw — qualquer envelope adicional seria overhead puro. |
| 3 | **pgzip + Zstd** | gzip stdlib, LZ4 | pgzip para compressão paralela (klauspost). Zstd para alta razão de compressão. Selecionável por storage via `compression_mode`. |
| 4 | **Ring buffer em memória** | Write-ahead log em disco | Simplicidade e performance. Disco adicionaria I/O na origem — contradiz o princípio zero-footprint. |
| 5 | **Escrita atômica** (`.tmp` + rename) | Escrita direta, journaling | Rename é atômico no Linux. Garante que um backup parcial nunca substitui um completo. |
| 6 | **Rotação por índice** (N mais recentes) | Rotação por tempo, GFS | Simplicidade. Rotação por tempo pode ser implementada futuramente. |
| 7 | **`slog` (stdlib)** | Zap, Zerolog, Logrus | Zero dependências externas. Performance adequada. JSON structured por padrão. |
| 8 | **Named Storages** | Storage único | Permite políticas de rotação independentes por tipo de backup. |
| 9 | **SPA embarcado** (`go:embed`) | Grafana, Prometheus UI | Zero dependências externas. Single binary deployment. |
| 10 | **Chunk shard levels configurável** (1 ou 2) | Estrutura flat única | 1 nível é suficiente para a maioria. 2 níveis reduz contagem de entradas por diretório em sessões com muitos chunks paralelos, melhorando performance do filesystem. |
| 11 | **Persistência JSONL da WebUI** | SQLite, banco em memória | JSONL é append-only, zero dependências, rotação por `max_lines`. Sobrevive a crashes sem corrupção. |
| 12 | **Slot struct pré-alocado** (v3.0.0) | 12× `sync.Map` | Elimina overhead de type-assertion, permite métricas atômicas tipadas por slot e estado de vida explícito. |

---

## 11. Referências

- [[Especificação Técnica|Especificacao-Tecnica]] — Detalhes completos do protocolo binário, frames e sessão
- [[Instalacao]] — Build, PKI/mTLS, configuração, systemd
- [[Guia de Uso|Guia-de-Uso]] — Comandos CLI, daemon, retry, rotação, troubleshooting, WebUI
