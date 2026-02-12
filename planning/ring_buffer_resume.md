# Ring Buffer com Resume — Plano de Implementação

Backups de 700GB falham em ~400GB por instabilidade de rede. O objetivo é permitir que o agent retome o envio a partir do último offset confirmado pelo server, usando um ring buffer em memória com backpressure.

## Arquitetura

```
Agent:
  scan → tar → gzip → [hash+count] → [Ring Buffer 256MB] → TLS → Server
                                           │                  ▲
                                           │  backpressure     │
                                           │  (bloqueia se     │
                                           │   buffer cheio)   │
                                           │                   │
                                           ▼                   │
                                     Server ACK ◀──────────────┘
                                     (offset confirmado)
```

### Fluxo Normal (sem queda)

```
Agent                                         Server
  │── HANDSHAKE (agent, storage, ver) ──────▶│
  │◀── ACK (GO, sessionID) ────────────────  │
  │                                           │
  │── DATA chunks (via ring buffer) ────────▶│
  │◀── SACK (offset=64MB) ────────────────   │  ← a cada 64MB
  │◀── SACK (offset=128MB) ───────────────   │
  │     ...                                   │
  │── TRAILER (SHA-256, size) ──────────────▶│
  │◀── FINAL ACK ──────────────────────────  │
```

### Fluxo com Queda e Resume

```
Agent                                         Server
  │── DATA ... ─────────────────────────────▶│
  │◀── SACK (offset=400GB) ────────────────  │
  │     ✕ conexão cai ✕                       │
  │                                           │  (server mantém .tmp)
  │── RESUME (agent, storage, sessionID) ──▶ │
  │◀── RESUME_ACK (lastOffset=400GB) ─────  │
  │                                           │
  │── DATA (a partir de 400GB do buffer) ──▶ │
  │◀── SACK (offset=464MB) ───────────────   │
  │     ...                                   │
  │── TRAILER ─────────────────────────────▶ │
  │◀── FINAL ACK ──────────────────────────  │
```

### Quando o buffer não cobre

Se `(bytesProduzidos - lastServerACK) > bufferSize`, o resume é impossível. O agent descarta a sessão e recomeça do zero.

---

## Proposed Changes

### Ring Buffer (Agent)

Estrutura de dados circular thread-safe com backpressure.

#### [NEW] [ringbuffer.go](file:///home/lucas/Projects/n-backup/internal/agent/ringbuffer.go)

- `RingBuffer` struct: buffer circular de tamanho configurável
- `Write(p []byte) (int, error)` — bloqueia quando cheio (backpressure)
- `ReadAt(offset int64, p []byte) (int, error)` — lê a partir de offset absoluto
- `Advance(offset int64)` — avança tail até o offset ACK'd (libera espaço)
- `Contains(offset int64) bool` — verifica se offset está no buffer
- `Close()` — sinaliza fim de produção
- Campos: `buf []byte`, `head/tail int64` (offsets absolutos), `mu sync.Mutex`, `notFull/notEmpty sync.Cond`

#### [NEW] [ringbuffer_test.go](file:///home/lucas/Projects/n-backup/internal/agent/ringbuffer_test.go)

- `TestRingBuffer_WriteRead` — escrita e leitura básica
- `TestRingBuffer_Backpressure` — Write bloqueia quando cheio
- `TestRingBuffer_Advance` — libera espaço após ACK
- `TestRingBuffer_Contains` — verifica range válido
- `TestRingBuffer_Overflow` — detecta que offset não está mais no buffer

---

### Protocolo

Novos frames para session tracking, SACK periódico e RESUME.

#### [MODIFY] [frames.go](file:///home/lucas/Projects/n-backup/internal/protocol/frames.go)

- Novo magic: `MagicResume = [4]byte{'R', 'S', 'M', 'E'}` e `MagicSACK = [4]byte{'S', 'A', 'C', 'K'}`
- Novo struct `Resume`: `SessionID string`, `AgentName string`, `StorageName string`
- Novo struct `SACK`: `Offset uint64`
- Novo struct `ResumeACK`: `Status byte`, `LastOffset uint64`
- Constantes: `ResumeStatusOK byte = 0x00`, `ResumeStatusNotFound byte = 0x01`
- Alterar `ACK` para incluir `SessionID string` (server gera UUID na sessão nova)

#### [MODIFY] [writer.go](file:///home/lucas/Projects/n-backup/internal/protocol/writer.go)

- `WriteResume(w, sessionID, agentName, storageName)` — frame RESUME
- `WriteSACK(w, offset)` — frame SACK (8 bytes offset)
- `WriteResumeACK(w, status, lastOffset)` — resposta ao RESUME

#### [MODIFY] [reader.go](file:///home/lucas/Projects/n-backup/internal/protocol/reader.go)

- `ReadResume(r)` — lê frame RESUME
- `ReadSACK(r)` — lê frame SACK
- `ReadResumeACK(r)` — lê frame RESUME_ACK

#### [MODIFY] [protocol_test.go](file:///home/lucas/Projects/n-backup/internal/protocol/protocol_test.go)

- Round-trip tests para Resume, SACK, ResumeACK

---

### Config

#### [MODIFY] [agent.go](file:///home/lucas/Projects/n-backup/internal/config/agent.go)

- Novo campo em `AgentConfig`: `Resume ResumeConfig`
- `ResumeConfig`: `BufferSize string` (ex: `"256mb"`, default `"256mb"`)
- Parser de human-readable size (`256mb` → `268435456`)

#### [MODIFY] [config_test.go](file:///home/lucas/Projects/n-backup/internal/config/config_test.go)

- Testes para parse de `BufferSize` e default value

---

### Agent — Backup com Resume

#### [MODIFY] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

Refatoração significativa — `RunBackup` vira uma state machine:

1. **Fase 1: Pipeline** — Inicia `scan → tar → gzip → ringBuffer` em goroutine (produtor)
2. **Fase 2: Conexão** — Conecta ao server, envia HANDSHAKE, recebe ACK com `sessionID`
3. **Fase 3: Sender** — Goroutine que lê do ring buffer e envia via TLS (consumidor)
4. **Fase 4: ACK Reader** — Goroutine que lê SACKs do server e chama `ringBuffer.Advance(offset)`
5. **Fase 5: Reconexão** — Se sender/ACK reader detectam erro de rede:
   - Tenta reconectar (com backoff)
   - Envia RESUME com sessionID
   - Se server aceita: resume do lastOffset
   - Se server rejeita ou buffer overflow: restart from scratch
6. **Fase 6: Trailer** — Quando produtor termina, sender envia trailer + lê final ACK

#### [MODIFY] [streamer.go](file:///home/lucas/Projects/n-backup/internal/agent/streamer.go)

- `Stream()` agora escreve no ring buffer (que implementa `io.Writer`) em vez de escrever direto na conn
- Backpressure é natural: `ringBuffer.Write()` bloqueia quando cheio

---

### Server — Session Tracking e SACKs

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- `handleBackup()`:
  - Gera `sessionID` (UUID) e inclui no ACK
  - Armazena sessão: `sessions.Store(sessionID, &PartialSession{tmpPath, bytesWritten})`
  - Envia **SACK a cada 64MB** escritos (goroutine separada ou inline)
  - Se conexão cai: mantém `.tmp` e sessão no mapa
- Novo `handleResume()`:
  - Lê frame RESUME
  - Busca sessão pelo sessionID
  - Verifica que `.tmp` existe e tamanho confere
  - Responde `ResumeACK(OK, lastOffset)` ou `ResumeACK(NotFound, 0)`
  - Continua recebendo dados a partir do offset
- **Cleanup**: goroutine periódica que remove sessões com `.tmp` > 1h sem atividade
- `HandleConnection`: reconhece magic `RSME` como RESUME (além de `NBKP` e `PING`)

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/server/server.go)

- Novo `sessions *sync.Map` passado ao Handler
- Goroutine de cleanup de sessões expiradas (TTL configurável, default 1h)

---

### Testes de Integração

#### [MODIFY] [integration_test.go](file:///home/lucas/Projects/n-backup/internal/integration/integration_test.go)

- `TestEndToEnd_ResumeAfterDisconnect` — simula queda no meio do stream e resume bem-sucedido
- `TestEndToEnd_ResumeBufferOverflow` — simula queda longa onde buffer não cobre → restart
- `TestEndToEnd_ResumeSessionExpired` — simula resume após sessão ter expirado no server

---

### Documentação

#### [MODIFY] [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md)

- Novos frames: RESUME, SACK, RESUME_ACK
- Diagrama de sessão com resume

#### [MODIFY] [usage.md](file:///home/lucas/Projects/n-backup/docs/usage.md)

- Seção sobre resume e configuração do buffer

---

## Configuração Resultante

```yaml
# Agent
resume:
  buffer_size: 256mb    # Ring buffer size (default: 256mb, max recomendado: 1gb)
```

---

## Verification Plan

### Automated Tests
- `go test ./... -v -count=1` — todos os testes existentes + novos
- Testes E2E com desconexão simulada via `net.Pipe()` + close forçado

### Manual Verification
- Deploy nos hosts reais (popline-01 → backup.nishisan.dev)
- Simular queda com `iptables -A OUTPUT -p tcp --dport 9847 -j DROP` e reabilitar após 5s
- Verificar no log que o resume aconteceu e o backup completou

---

## Ordem de Implementação

1. Ring buffer (structure de dados isolada + testes)
2. Protocolo (frames novos + testes)
3. Config (parse buffer_size)
4. Server (session tracking, SACK, RESUME handler, cleanup)
5. Agent (refatorar backup.go como state machine, integrar ring buffer)
6. Testes E2E
7. Documentação
