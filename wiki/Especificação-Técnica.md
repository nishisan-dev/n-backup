# Especificação Técnica — n-backup

## 1. Visão Geral

O **n-backup** é um sistema de backup client-server escrito em Go, projetado para realizar streaming de dados diretamente da origem para o destino **sem criar arquivos temporários no disco de origem**. O resultado é um arquivo `.tar.gz` padrão Linux, preservando caminhos, permissões e estrutura de diretórios.

### 1.1 Problema

Em servidores com pouco espaço em disco ou discos lentos, o método tradicional ("compactar primeiro, enviar depois") é inviável para grandes volumes. O n-backup elimina essa limitação ao fazer `tar | pgzip | network` em um único pipeline de streaming.

### 1.2 Origem

Evolução do script shell abaixo para um binário Go com mTLS, resiliência e operação autônoma:

```bash
tar -cvf - -C / app/scripts/ home/ etc/ | gzip | ssh "$REMOTE_HOST" "cat > '$REMOTE_DIR/$TMP_FILE'"
```

---

## 2. Decisões de Arquitetura

### 2.1 Transporte: TCP Puro + mTLS

| Decisão | Justificativa |
|---------|---------------|
| **TCP puro** em vez de HTTP/2 ou gRPC | Fluxo unidirecional (Client → Server) sem necessidade de multiplexação, content negotiation ou middleware HTTP. Zero overhead por byte. |
| **TLS 1.3 com Mutual Auth (mTLS)** | Autenticação mútua obrigatória. Sem SSH keys, sem shell remoto. |
| **Protocolo binário customizado** | Header mínimo (~60 bytes por sessão). O payload é o stream raw do tar+gzip. |

### 2.2 Formato de Saída

- **`.tar.gz` padrão** — compatível com `tar xzf` do Linux.
- O server **não descompacta** — grava o stream raw diretamente em disco.
- Preserva: caminhos relativos, permissões, ownership, symlinks.

### 2.3 Streaming Pipeline

```
fs.WalkDir ──▶ tar.Writer ──▶ pgzip.Writer ──▶ ThrottledWriter ──▶ RingBuffer ──▶ tls.Conn ──▶ Server (io.Copy → disk)
     │                                    │             │
     └── excludes/includes (glob)          │             └── backpressure (bloqueia se cheio)
                                           └── rate.Limiter (Token Bucket, golang.org/x/time/rate)
```

- O RingBuffer implementa `io.Writer` e aplica backpressure quando cheio.
- Sender goroutine lê do buffer por offset absoluto e envia para a conexão.
- ACK reader processa SACKs do server e avança o tail do buffer.
- SHA-256 é calculado inline via `io.MultiWriter` sobre o stream compactado.
- Se a conexão cair, o sender reconecta e retoma do último offset válido.
- A compressão usa `pgzip` (klauspost) com goroutines paralelas — até 3x mais rápido que gzip stdlib.

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
┌──────────┬──────┬──────────────────┬───────┬───────────────────┬───────┬───────────────────┬───────┬────────────────────┬───────┐
│ "NBKP"   │ Ver  │ AgentName (UTF8) │ '\n'  │ StorageName (UTF8) │ '\n'  │ BackupName (UTF8)  │ '\n'  │ ClientVersion (UTF8)│ '\n'  │
│ 4 bytes  │ 1B   │ variável         │ 1B    │ variável           │ 1B    │ variável           │ 1B    │ variável            │ 1B    │
└──────────┴──────┴──────────────────┴───────┴───────────────────┴───────┴───────────────────┴───────┴────────────────────┴───────┘
```

- **Magic**: `0x4E 0x42 0x4B 0x50` ("NBKP")
- **Ver**: Versão do protocolo (`0x03` — v3 com ClientVersion)
- **AgentName**: Identificador UTF-8 do agent, delimitado por `\n`
- **StorageName**: Nome do storage de destino no server, delimitado por `\n`
- **BackupName**: Nome do backup entry, delimitado por `\n`
- **ClientVersion**: Versão do binário do agent (ex: `v1.7.0`), delimitado por `\n`

> **Hardening (v1.7.0+):** Leituras de campos delimitados por `\n` utilizam `readLineLimited` com máximo de 1024 bytes, prevenindo ataques de OOM ou slowloris via linhas infinitas.

#### ACK (Server → Client)

```
┌──────────┬──────────────────┬───────┬────────────────┬───────┐
│ Status   │ Message (UTF8)   │ '\n'  │ SessionID (UTF8)│ '\n'  │
│ 1 byte   │ variável (opt)   │ 1B    │ variável (opt)  │ 1B    │
└──────────┴──────────────────┴───────┴────────────────┴───────┘
```

| Status | Código | Significado |
|--------|--------|-------------|
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
└──────────┴─────────────────────────┴───────────┘
```

#### Final ACK (Server → Client)

```
┌──────────┐
│ Status   │
│ 1 byte   │
└──────────┘
```

| Status | Código | Significado |
|--------|--------|-------------|
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
|--------|--------|-------------|
| OK | `0x00` | Resume aceito, continuar do LastOffset |
| NOT_FOUND | `0x01` | Sessão expirada ou inválida, reiniciar |

#### SACK (Server → Client)

```
┌──────────┬─────────────┐
│ "SACK"   │ Offset        │
│ 4 bytes  │ 8B uint64     │
└──────────┴─────────────┘
```

Enviado periodicamente pelo server (a cada 1MB) para confirmar recebimento. O agent avança o tail do ring buffer, liberando espaço para novas escritas.

### 3.5 Parallel Streaming

Para backups grandes, o agent pode usar **múltiplos streams paralelos** para aumentar o throughput.

#### Fluxo (v1.2.3+)

A conexão primária é **control-only** — não transporta dados de stream. Todos os N streams (incluindo stream 0) conectam via `ParallelJoin` em conexões TLS separadas.

```
Client                                     Server
  │                                          │
  │──── Handshake + ACK GO (normal) ───────▶│  ← conn primária (control-only)
  │──── ParallelInit (maxStreams, chunkSize)▶│
  │                                          │
  │──── ParallelJoin (sessionID, idx=0) ──▶ │  ← nova conn TLS (stream 0)
  │◀─── ParallelACK (OK, lastOffset=0) ──── │
  │──── [ChunkHeader+DATA] stream 0 ──────▶ │
  │                                          │
  │──── ParallelJoin (sessionID, idx=1) ──▶ │  ← nova conn TLS (stream 1)
  │◀─── ParallelACK (OK, lastOffset=0) ──── │
  │──── [ChunkHeader+DATA] stream 1 ──────▶ │
  │                                          │
  │     ... streams completam ...             │
  │                                          │
  │──── Trailer (SHA-256, size total) ─────▶ │  ← via conn primária (sem ChunkHeader)
  │◀─── FINAL ACK ─────────────────────── │
```

#### Stream Resume (Re-Join)

Se uma conexão de stream cai mid-transfer, o agent reconecta via novo `ParallelJoin` para o mesmo `StreamIndex`. O server responde com o `lastOffset` já recebido desse stream, permitindo resume sem reenvio de dados.

```
Client                                     Server
  │   (stream 1 cai — i/o timeout)           │
  │                                          │
  │   ... retry com exponential backoff ...   │
  │                                          │
  │──── ParallelJoin (sessionID, idx=1) ──▶ │  ← reconexão
  │◀─── ParallelACK (OK, lastOffset=N) ──── │  ← resume do offset N
  │──── [ChunkHeader+DATA] from offset N ──▶ │
```

O agent faz até **3 tentativas** de reconnect por stream com backoff exponencial (1s, 2s, 4s). Se todas falharem, o stream é marcado como **permanentemente morto**. O backup continua nos streams restantes. Se todos os streams morrerem, o backup falha com `ErrAllStreamsDead`.

#### ChunkHeader Framing

Nos streams paralelos, cada chunk é precedido por um header:

```
┌────────────┬──────────┬──────────┐
│ StreamIndex │ ChunkSeq  │ DataLen   │
│ 1 byte      │ 4B uint32 │ 4B uint32 │
└────────────┴──────────┴──────────┘
```

Seguido por `DataLen` bytes de payload. O server usa `StreamIndex` e `ChunkSeq` para reassemblar na ordem correta.

#### ParallelInit (Client → Server)

Enviado imediatamente após o ACK GO na conexão primária:

```
┌──────────┬───────────┐
│ MaxStreams│ ChunkSize   │
│ 1 byte   │ 4B uint32   │
└──────────┴───────────┘
```

- **MaxStreams**: Número máximo de streams (1-255)
- **ChunkSize**: Tamanho de cada chunk em bytes (default: 1MB)

#### ParallelJoin (Client → Server)

Enviado em uma **nova conexão TLS** para unir-se a uma sessão existente:

```
┌──────────┬────────────────┬───────┬────────────┐
│ "PJIN"   │ SessionID (UTF8)│ '\n'  │ StreamIndex │
│ 4 bytes  │ variável        │ 1B    │ 1 byte      │
└──────────┴────────────────┴───────┴────────────┘
```

Usado tanto para **first-join** quanto para **re-join** (resume após queda).

#### ParallelACK (Server → Client)

```
┌──────────┬─────────────┐
│ Status   │ LastOffset    │
│ 1 byte   │ 8B uint64     │
└──────────┴─────────────┘
```

Total: **9 bytes**.

| Status | Código | Significado |
|--------|--------|-------------|
| OK | `0x00` | Stream aceito. `LastOffset` = bytes já recebidos (0 para first-join) |
| FULL | `0x01` | Sessão no limite de streams |
| NOT_FOUND | `0x02` | SessionID não encontrado |

#### ChunkSACK (Server → Client)

ACK seletivo por stream, enviado nos streams de dados:

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
    auto_scaler: adaptive # efficiency (padrão) ou adaptive
```

**No server**, cada storage pode configurar o nível de sharding para staging de chunks:

```yaml
storages:
  home-dirs:
    assembler_mode: lazy
    chunk_shard_levels: 2  # 1 (flat, padrão) ou 2 (2 níveis de subdiretórios)
```

- **parallels**: `0` desabilita (single stream), `1-255` define o máximo de streams.
- **auto_scaler**: `efficiency` (threshold-based, padrão) ou `adaptive` (probe-and-measure).
- **bandwidth_limit**: limite de upload em Bytes/segundo (ex: `50mb`, `1gb`). Mínimo: `64kb`. Vazio = sem limite.
- **chunk_shard_levels**: `1` (padrão, flat) ou `2` (2 níveis de subdiretórios) — controla a organização dos chunks no staging do assembler.

### 3.6 Control Channel Protocol (v1.3.8+)

O agent pode manter uma conexão TLS persistente dedicada ao controle e monitoramento.

#### Estabelecimento

```
Agent                                     Server
  │                                          │
  │──── TLS 1.3 Handshake (mTLS) ──────────▶│
  │──── CTRL (4B: 0x43 0x54 0x52 0x4C) ────▶│  ← magic de controle
  │──── KeepAliveInterval (uint32, secs) ──▶│  ← negociação de intervalo
  │                                          │
  │──── ControlPing (CPNG + timestamp) ────▶│  ← keep-alive periódico
  │◀─── ControlPong (CPNG + ts + load +    ─│  ← resposta + status do server
  │                    diskFree)              │
  │     ... ping/pong contínuo ...           │
  │                                          │
  │◀─── ControlRotate (CROT + streamIdx) ──│  ← server pede drenagem
  │──── ControlRotateACK (CRAK + streamIdx)▶│  ← agent confirma drenagem
```

#### Frames de Controle

##### ControlPing (Agent → Server)

```
┌──────────┬─────────────┐
│ "CPNG"   │ Timestamp    │
│ 4 bytes  │ 8B int64     │
└──────────┴─────────────┘
```

- **Magic**: `0x43 0x50 0x4E 0x47` ("CPNG")
- **Timestamp**: `time.Now().UnixNano()` — usado para cálculo de RTT

##### ControlPong (Server → Agent)

```
┌──────────┬─────────────┬────────────┬──────────┐
│ "CPNG"   │ Timestamp    │ ServerLoad  │ DiskFree  │
│ 4 bytes  │ 8B int64     │ 4B float32  │ 4B uint32 │
└──────────┴─────────────┴────────────┴──────────┘
```

- **Timestamp**: echo do timestamp do ping (para cálculo de RTT)
- **ServerLoad**: carga do server (0.0 a 1.0)
- **DiskFree**: espaço livre em disco (MB)

##### ControlRotate (Server → Agent)

```
┌──────────┬────────────┐
│ "CROT"   │ StreamIndex │
│ 4 bytes  │ 1 byte      │
└──────────┴────────────┘
```

Solicita drenagem graceful do stream indicado.

##### ControlRotateACK (Agent → Server)

```
┌──────────┬────────────┐
│ "CRAK"   │ StreamIndex │
│ 4 bytes  │ 1 byte      │
└──────────┴────────────┘
```

Confirma que o stream foi drenado e pode ser rotacionado.

##### ControlAdmit (Server → Agent)

```
┌──────────┬────────┐
│ "CADM"   │ SlotID  │
│ 4 bytes  │ 1 byte  │
└──────────┴────────┘
```

Autoriza início de backup em slot específico.

##### ControlDefer (Server → Agent)

```
┌──────────┬─────────────┐
│ "CDFE"   │ WaitMinutes  │
│ 4 bytes  │ 4B uint32    │
└──────────┴─────────────┘
```

Solicita que o agent espere antes de iniciar backup.

##### ControlAbort (Server → Agent)

```
┌──────────┬──────────┐
│ "CABT"   │ Reason    │
│ 4 bytes  │ 4B uint32 │
└──────────┴──────────┘
```

| Reason | Código | Significado |
|--------|--------|-------------|
| DISK_FULL | `1` | Disco cheio no server |
| SERVER_BUSY | `2` | Server sobrecarregado |
| MAINTENANCE | `3` | Server em manutenção |

##### ControlProgress (Agent → Server)

```
┌──────────┬──────────────┬────────────┬──────────────┐
│ "CPRG"   │ TotalObjects  │ ObjectsSent │ WalkComplete  │
│ 4 bytes  │ 4B uint32     │ 4B uint32   │ 1 byte        │
└──────────┴──────────────┴────────────┴──────────────┘
```

- **TotalObjects**: Total de objetos a enviar (0 se PreScan ainda não completou)
- **ObjectsSent**: Objetos já processados pelo pipeline
- **WalkComplete**: `0x01` se PreScan completou e `TotalObjects` é confiável

Enviado periodicamente pelo agent junto com ControlPing. O server popula os dados na `ParallelSession` para cálculo de progresso e ETA na [[WebUI]].

##### ControlAutoScaleStats (Agent → Server) (v2.1.2+)

```
┌──────────┬────────────┬─────────────┬──────────┬──────────────┬────────────┬───────┬─────────────┐
│ "CASS"   │ Efficiency  │ ProducerMBs  │ DrainMBs  │ ActiveStreams │ MaxStreams  │ State │ ProbeActive │
│ 4 bytes  │ 4B float32  │ 4B float32   │ 4B float32│ 1 byte       │ 1 byte     │ 1B    │ 1 byte      │
└──────────┴────────────┴─────────────┴──────────┴──────────────┴────────────┴───────┴─────────────┘
```

- **Efficiency**: razão producer/drain (> 1.0 = produzindo mais rápido que drenando)
- **ProducerMBs / DrainMBs**: throughput em MB/s
- **ActiveStreams / MaxStreams**: streams em uso e limite configurado
- **State**: `0` = Stable, `1` = ScalingUp, `2` = ScaleDown, `3` = Probing
- **ProbeActive**: `1` se há um probe de stream em andamento

##### ControlIngestionDone / CIDN (Agent → Server) (v2.5+)

```
┌──────────┬─────────────────┬──────────────────┐
│ "CIDN"   │ SessionIDLen    │ SessionID (UTF8) │
│ 4 bytes  │ 1 byte          │ até 255 bytes    │
└──────────┴─────────────────┴──────────────────┘
```

- **Magic**: `0x43 0x49 0x44 0x4E` ("CIDN")
- **SessionIDLen**: comprimento em bytes do SessionID (1 byte, valor 0-255)
- **SessionID**: UUID da sessão, mesmo valor recebido no ACK do Handshake

Sinaliza **explicitamente** ao server que o agent terminou de enviar todos os chunks com sucesso na sessão paralela. Permite que o server acione commit e rotação sem aguardar EOF/timeout.

Enviado pelo agent via canal de controle **imediatamente após o Trailer ser entregue** na sessão paralela.

#### RTT EWMA

O RTT é calculado via Exponentially Weighted Moving Average (α = 0.25):

```
RTT_new = α × sample + (1 - α) × RTT_old
```

#### Configuração

```yaml
daemon:
  control_channel:
    enabled: true              # default: true
    keepalive_interval: 30s    # intervalo entre PINGs (≥ 1s)
    reconnect_delay: 5s        # delay inicial de reconexão
    max_reconnect_delay: 5m    # delay máximo (exponential backoff)
```

---

## 4. Configuração

Veja [[Configuração de Exemplo|Configuração-de-Exemplo]] para os arquivos YAML completos e comentados.

---

## 5. Resiliência

### 5.1 Retry com Exponential Backoff

**Conexão inicial e health check:** backoff padrão com até `max_attempts` tentativas.

```
Tentativa 1 → falha → aguarda 1s
Tentativa 2 → falha → aguarda 2s
Tentativa 3 → falha → aguarda 4s
Tentativa 4 → falha → aguarda 8s (capped em max_delay)
Tentativa 5 → falha → ABORT, log error
```

**Streams paralelos individuais (v1.2.3+):** cada stream tem retry independente (até `maxRetriesPerStream=5` tentativas, backoff 1s/2s/4s/.../30s cap). Se um stream falha, os demais continuam operando. O backup só é abortado quando **todos os streams** estão mortos (`ErrAllStreamsDead`). Conexões TCP possuem **write deadline** para detecção de half-open connections.

> **Fix (v2.6.0):** O contador de retries é **resetado para zero** após cada reconexão bem-sucedida. Isso evita a morte prematura de streams em cenários de falhas intermitentes esporádicas: sem o reset, um stream que passa por 3 falhas intermitentes ao longo do tempo (mas sempre reconecta) seria marcado como morto mesmo estando operacional.

**Timeout de job:** o scheduler configura `context.WithTimeout(24h)` para cada job de backup. Isso previne zombie jobs.

### 5.2 Lock de Execução

O daemon garante que **apenas um backup por agent** é executado simultaneamente (mutex interno). O server também rejeita conexões duplicadas do mesmo agent (`BUSY`).

### 5.3 Atomicidade de Escrita

O server grava em arquivo temporário (`.tmp`) e só renomeia (atomic rename) após validação do checksum SHA-256.

### 5.4 Resume de Backups

Veja detalhes completos na seção 3.4 (Resume Protocol) e na seção 3.5 (Parallel Streaming — Stream Resume).

### 5.5 Graceful Shutdown

O daemon responde a `SIGTERM` e `SIGINT`:
- Se ocioso: shutdown imediato.
- Se backup em andamento: aguarda conclusão ou timeout configurável antes de abortar.

### 5.6 Control Channel (v1.3.8+)

Veja detalhes completos na seção 3.6 (Control Channel Protocol).

---

## 6. Stack Técnica

| Recurso | Tecnologia |
|---------|-----------|
| Linguagem | Go (Golang) |
| Transporte | TCP puro sobre TLS 1.3 |
| Segurança | mTLS (Mutual TLS) |
| Compactação | pgzip (`klauspost/pgzip` — compressão paralela multi-core) |
| Empacotamento | tar (`archive/tar` stdlib) |
| Configuração | YAML (`gopkg.in/yaml.v3`) |
| Logging | `slog` (stdlib Go 1.21+) |
| Scheduler | `robfig/cron` |
| Distribuição | Binário estático (GoReleaser) |

---

## 7. Fora do Escopo (v1)

- Restore via CLI (extrair manualmente com `tar xzf`)
- Backup incremental / diferencial
- Deduplicação
- PKI integrada (certificados gerenciados externamente na v1)
