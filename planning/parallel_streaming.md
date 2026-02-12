# Parallel Streaming com Auto-Scaler

## Problema

Um único TCP stream não satura a largura de banda disponível (congestion window, RTT). Com N streams paralelos, o throughput agregado escala até o limite do link.

## Design

### Modelo Conceitual

```
                           ┌──── RingBuffer[0] ──── conn[0] ──── Server chunk_0 ─┐
Producer ── Dispatcher ────┤                                                      ├── Assembler → .tmp → atomic rename
  (tar.gz)    (round-robin)├──── RingBuffer[1] ──── conn[1] ──── Server chunk_1 ─┤
                           ├──── RingBuffer[2] ──── conn[2] ──── Server chunk_2 ─┤
                           └──── RingBuffer[3] ──── conn[3] ──── Server chunk_3 ─┘
                                       ▲
                                       │
                                  Auto-Scaler
                              (efficiency metric)
```

- **Producer**: goroutine única (Scanner → tar → gzip) gera stream linear
- **Dispatcher**: distribui chunks de tamanho fixo (= buffer_size) em round-robin pelos N ring buffers
- **Cada RingBuffer**: tem sender goroutine + ACK reader próprios (reutiliza infra existente)
- **Auto-Scaler**: a cada 15s avalia `efficiency = producer_rate / (drain_rate × active_streams)`, com histerese de 3 janelas

### Auto-Scaler

| Condição | Ação |
|----------|------|
| `efficiency > 1.0` por 3 janelas | Scale-up: ativar +1 stream (até `max_parallels`) |
| `efficiency < 0.7` por 3 janelas | Scale-down: desativar 1 stream (mínimo 1) |
| `0.7 ≤ efficiency ≤ 1.0` | Manter estável |

Scale-down = stream idle (para de receber chunks). Scale-up = `PARALLEL_JOIN` para registrar novo stream na session.

### Protocolo

Novos frames:

```
PARALLEL_INIT (Client → Server, dentro do Handshake):
  [MaxStreams uint8 1B] [ChunkSize uint32 4B]

PARALLEL_JOIN (Client → Server, nova connection para session existente):
  Magic "PJIN" [4B] [Version 1B] [SessionID UTF-8 '\n'] [StreamIndex uint8 1B]
  Server responde: ACK com status

CHUNK_SACK (Server → Client, por stream):
  Magic "CSAK" [4B] [StreamIndex uint8 1B] [ChunkSeq uint32 4B] [Offset uint64 8B]
```

O Handshake existente é estendido (campo opcional `MaxStreams` quando > 0). ACK do server confirma parallel aceito.

### Server: Chunk Assembler

- Cada stream escreve em `{sessionID}_chunk_{streamIndex}.tmp`
- Manifest em memória: `[]ChunkMeta{ StreamIndex, ChunkSeq, Offset, Length }`
- Ao receber Trailer: ordena chunks por `ChunkSeq` → concatena → hash → atomic rename
- Cleanup de chunks individuais após concatenação

---

## Proposed Changes

### Fase 1: Config

#### [MODIFY] [agent.go](file:///home/lucas/Projects/n-backup/internal/config/agent.go)

```go
type BackupEntry struct {
    Name      string         `yaml:"name"`
    Storage   string         `yaml:"storage"`
    Sources   []BackupSource `yaml:"sources"`
    Exclude   []string       `yaml:"exclude"`
    Parallels int            `yaml:"parallels"` // 0=desabilitado, 1-8=máx streams
}
```

Validação: `0 ≤ parallels ≤ 8`, default `0` (single stream legado).

---

### Fase 2: Protocolo

#### [MODIFY] [frames.go](file:///home/lucas/Projects/n-backup/internal/protocol/frames.go)

- `MagicParallelJoin = [4]byte{'P','J','I','N'}`
- `MagicChunkSACK = [4]byte{'C','S','A','K'}`
- Struct `ParallelInit { MaxStreams uint8; ChunkSize uint32 }`
- Struct `ParallelJoin { SessionID string; StreamIndex uint8 }`
- Struct `ChunkSACK { StreamIndex uint8; ChunkSeq uint32; Offset uint64 }`

#### [MODIFY] [writer.go](file:///home/lucas/Projects/n-backup/internal/protocol/writer.go)

- `WriteParallelInit`, `WriteParallelJoin`, `WriteChunkSACK`

#### [MODIFY] [reader.go](file:///home/lucas/Projects/n-backup/internal/protocol/reader.go)

- `ReadParallelInit`, `ReadParallelJoin`, `ReadChunkSACK`

---

### Fase 3: Agent — Dispatcher

#### [NEW] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

`Dispatcher` struct:
- Recebe `io.Writer` interface (é o destino do `Stream()`)
- Internamente gerencia N `RingBuffer`, cada um com sender + ACK reader
- Método `Write(p []byte)` distribui chunks em round-robin
- Método `SetActiveStreams(n int)` para o auto-scaler
- Canal de métricas (`producerRate`, `drainRate[]`)

#### [NEW] [autoscaler.go](file:///home/lucas/Projects/n-backup/internal/agent/autoscaler.go)

`AutoScaler` struct:
- Goroutine com ticker de 15s
- Lê métricas do dispatcher
- Aplica histerese (3 janelas consecutivas)
- Chama `dispatcher.SetActiveStreams(n)`
- Log de eventos scale-up/down

#### [MODIFY] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

- Quando `entry.Parallels > 0`: cria `Dispatcher` em vez do `RingBuffer` avulso
- `Stream()` escreve no dispatcher (que implementa `io.Writer`)
- Resume: cada stream tem offset independente

---

### Fase 4: Server — Assembler

#### [NEW] [assembler.go](file:///home/lucas/Projects/n-backup/internal/server/assembler.go)

`ChunkAssembler` struct:
- Gerencia chunk files por session
- Manifest: `map[uint32]ChunkMeta` (chunkSeq → meta)
- `Assemble()`: concatena na ordem → hash → retorna path final
- Cleanup de chunk files

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- `handleBackup`: detecta `ParallelInit` no handshake → cria `ChunkAssembler`
- `handleParallelJoin`: registra stream secundário na session
- Cada stream escreve em chunk file separado com `ChunkSACK`
- `validateAndCommit`: chama `assembler.Assemble()` antes do hash final

---

### Fase 5: Testes e Documentação

- Testes unitários: dispatcher round-robin, auto-scaler histerese, assembler concatenação
- Teste integração: backup com parallels=2 e parallels=4
- Atualizar `specification.md` e `usage.md`

---

## Verification Plan

### Automated Tests
- `go test ./...` — todos os pacotes
- Teste integração com mock server aceitando N streams

### Manual Verification
- `nbackup-agent --once --progress` com `parallels: 4` e observar scale-up/down nos logs

---

## Estimativa

| Fase | Complexidade | Arquivos |
|------|-------------|----------|
| 1. Config | Baixa | 1 |
| 2. Protocolo | Média | 3 |
| 3. Dispatcher + Scaler | **Alta** | 3 (2 novos + 1 mod) |
| 4. Server Assembler | **Alta** | 2 (1 novo + 1 mod) |
| 5. Testes + Docs | Média | 4+ |
| **Total** | | ~13 arquivos |

> [!IMPORTANT]
> Esta feature é a mais complexa do projeto até agora. É recomendado dividir em sub-PRs: protocolo → dispatcher → assembler → auto-scaler.
