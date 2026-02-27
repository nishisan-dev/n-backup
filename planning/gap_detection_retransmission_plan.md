# Detecção de Gaps e Retransmissão de Chunks Perdidos

Backups paralelos longos (~33GB, ~2h, 12 streams) falham com `missing chunk seq N in lazy assembly` porque chunks perdidos durante timeouts de stream não são detectados nem retransmitidos. O server só descobre o buraco horas depois no `Finalize()`. Esta implementação adiciona detecção proativa de gaps no server, notificação via NACK ao agent, e retransmissão ou abort early.

## User Review Required

> [!IMPORTANT]
> **Mapeamento globalSeq→stream/offset:** O Dispatcher atual distribui chunks em round-robin mas **não mantém tabela inversa**. Cada ring buffer opera por offsets absolutos, não por globalSeq. A solução propõe um `chunkMap` (map[uint32]chunkLocation) no Dispatcher. Para 33K chunks (~1MB/chunk, 33GB), isso ocupa ~800KB de memória — aceitável. A tabela é consultada apenas no caminho de NACK (raro), não impacta performance normal.

> [!WARNING]
> **Compatibilidade:** O frame `ControlNACK` é novo. Agents com versão < v2.9 não interpretam este magic e vão desconectar o control channel (case `default` no pingLoop retorna). O server **deve verificar ClientVersion antes de enviar NACK**. Se a versão for incompatível, o comportamento permanece idêntico ao v2.8.4 (descoberta do gap só no Finalize).

> [!CAUTION]
> **Sem `bandwidth_limit` configurado**, o throughput é imprevisível e o ring buffer pode ser sobrescrito antes do NACK chegar. Neste caso, a retransmissão falha e o abort early é acionado. Isso é **melhor** que o cenário atual (descobrir 11h depois), mas o dimensional do buffer continua sendo responsabilidade do operador.

## Proposed Changes

### Protocol Layer

Novos frames de controle para comunicação bidirecional de gap/retransmissão via control channel.

#### [MODIFY] [control.go](file:///home/g0004218/Projects/n-backup/internal/protocol/control.go)

Adicionar:
- `MagicControlNACK = [4]byte{'C','N','A','K'}` — Server → Agent
- `MagicControlRetransmitResult = [4]byte{'C','R','T','R'}` — Agent → Server
- Struct `ControlNACK { MissingSeq uint32, SessionID string }` — Formato: `[Magic 4B] [MissingSeq uint32 4B] [SessionID UTF-8 '\n']`
- Struct `ControlRetransmitResult { MissingSeq uint32, Success bool }` — Formato: `[Magic 4B] [MissingSeq uint32 4B] [Success uint8 1B]`
- Funções `WriteControlNACK`, `ReadControlNACKPayload`, `WriteControlRetransmitResult`, `ReadControlRetransmitResultPayload`

> O `SessionID` no NACK é necessário porque um agent pode ter múltiplas sessões (futuro) e o server precisa discriminar. O `ControlRetransmitResult` permite ao server saber imediatamente se deve abortar.

---

### Server — Gap Tracker

Novo componente que detecta chunks faltantes de forma proativa.

#### [NEW] [gap_tracker.go](file:///home/g0004218/Projects/n-backup/internal/server/gap_tracker.go)

```go
type GapTracker struct {
    sessionID     string
    received      map[uint32]bool  // chunks recebidos
    maxSeenSeq    uint32           // maior globalSeq visto
    gapTimeout    time.Duration    // tempo de tolerância (default: 2x streamReadDeadline = 60s)
    firstSeen     map[uint32]time.Time // quando o gap foi detectado
    notifiedGaps  map[uint32]bool  // gaps já notificados (evita duplicata)
    mu            sync.Mutex
    logger        *slog.Logger
}

func NewGapTracker(sessionID string, gapTimeout time.Duration, logger *slog.Logger) *GapTracker
func (gt *GapTracker) RecordChunk(globalSeq uint32)  // chamado a cada chunk recebido
func (gt *GapTracker) CheckGaps() []uint32           // retorna gaps persistentes (>gapTimeout)
func (gt *GapTracker) ResolveGap(globalSeq uint32)   // chamado quando chunk retransmitido chega
func (gt *GapTracker) PendingGaps() int              // número de gaps não resolvidos
```

**Lógica de detecção:**
- A cada `RecordChunk(N)`: se `N > maxSeenSeq+1`, marca `maxSeenSeq+1..N-1` como gaps potenciais com timestamp `now()`
- `CheckGaps()` retorna apenas gaps com `time.Since(firstSeen) > gapTimeout`
- `received` usa map (não bitmap) porque os seqs podem ser esparsos em cenários de multi-stream
- `maxNACKsPerCycle` (default: 5) limita quantos NACKs são enviados por tick para evitar flood

#### [NEW] [gap_tracker_test.go](file:///home/g0004218/Projects/n-backup/internal/server/gap_tracker_test.go)

Testes unitários:
- Gap detectado: recebe seq 0,1,3 → CheckGaps reporta seq 2 após timeout
- Gap transiente: recebe seq 0,1,3,2 → gap resolvido, CheckGaps vazio
- Gap múltiplo: recebe seq 0,5 → 4 gaps detectados
- Limite maxNACKsPerCycle: com 10 gaps, retorna apenas 5
- Thread safety: RecordChunk concorrente

---

### Server — Integração

Instrumentar o handler para usar GapTracker e enviar NACKs pelo control channel.

#### [MODIFY] [handler.go](file:///home/g0004218/Projects/n-backup/internal/server/handler.go)

1. **ParallelSession** — adicionar campo:
   ```go
   GapTracker *GapTracker
   ```

2. **handleParallelBackup** — inicializar GapTracker ao criar a sessão:
   ```go
   gapTracker := NewGapTracker(sessionID, h.cfg.GapDetection.Timeout, logger)
   pSession.GapTracker = gapTracker
   ```

3. **receiveParallelStream** — após `chunk_received`, chamar:
   ```go
   session.GapTracker.RecordChunk(hdr.GlobalSeq)
   ```

4. **Goroutine de gap check** — nova goroutine `gapCheckLoop` por sessão, lançada em `handleParallelBackup`:
   ```go
   go h.gapCheckLoop(ctx, pSession)
   ```
   - Tick a cada 5s (configurável)
   - Chama `pSession.GapTracker.CheckGaps()`
   - Para cada gap, envia `ControlNACK` pelo control channel (usando `h.controlConns` + `h.controlConnsMu`)
   - Verifica ClientVersion antes de enviar (>= v2.9)
   - Loga "nack_sent" com session, missingSeq
   - Adicionar `controlConnMinVersion` como guard de version check

5. **handleControlChannel** — adicionar case `MagicControlRetransmitResult` no switch:
   - Se `Success == true`: chama `pSession.GapTracker.ResolveGap(missingSeq)`
   - Se `Success == false`: aciona abort early da sessão (emite evento, fecha streams, sinaliza assembler)

---

### Agent — Chunk Map (Dispatcher)

O Dispatcher precisa mapear globalSeq→{streamIndex, rbOffset, length} para localizar chunks no ring buffer.

#### [MODIFY] [dispatcher.go](file:///home/g0004218/Projects/n-backup/internal/agent/dispatcher.go)

1. **Nova struct e campo** no Dispatcher:
   ```go
   type chunkLocation struct {
       streamIdx int
       rbOffset  int64  // offset absoluto do ChunkHeader no ring buffer do stream
       length    int64  // ChunkHeaderSize + len(data)
   }
   
   // No Dispatcher:
   chunkMap   map[uint32]chunkLocation  // globalSeq → localização no ring buffer
   chunkMapMu sync.RWMutex
   ```

2. **emitChunk** — após escrever no ring buffer, registrar no chunkMap:
   ```go
   // Antes do Write do header:
   headerOffset := stream.rb.Head()
   // Após os Writes:
   d.chunkMapMu.Lock()
   d.chunkMap[seq] = chunkLocation{
       streamIdx: int(stream.index),
       rbOffset:  headerOffset,
       length:    int64(protocol.ChunkHeaderSize) + int64(len(data)),
   }
   d.chunkMapMu.Unlock()
   ```

3. **Novo método `RetransmitChunk`:**
   ```go
   func (d *Dispatcher) RetransmitChunk(globalSeq uint32) (bool, error)
   ```
   - Lookup no chunkMap → {streamIdx, rbOffset, length}
   - Verifica `stream.rb.ContainsRange(rbOffset, length)`
   - Se contém: lê do ring buffer via `ReadAt`, reenvia por qualquer stream ativo
   - Se não contém (sobrescrito): retorna `false, nil` → caller deve abortar

4. **Limpeza do chunkMap:** Quando o ring buffer avança (SACK via `startACKReader`), entries cujo rbOffset < stream.rb.Tail() podem ser removidas. Implementar lazy cleanup periódico (a cada 1000 SACKs ou 10s).

---

### Agent — NACK Handler (Control Channel)

#### [MODIFY] [control_channel.go](file:///home/g0004218/Projects/n-backup/internal/agent/control_channel.go)

1. **Novo callback** `onNACK`:
   ```go
   onNACK func(missingSeq uint32, sessionID string)
   ```

2. **Setter** `SetOnNACK(fn func(missingSeq uint32, sessionID string))`

3. **pingLoop reader** — adicionar case `MagicControlNACK`:
   ```go
   case protocol.MagicControlNACK:
       nack, err := protocol.ReadControlNACKPayload(conn)
       if err != nil {
           cc.logger.Warn("control channel: reading NACK payload", "error", err)
           return
       }
       cc.logger.Info("control channel: received ControlNACK",
           "missingSeq", nack.MissingSeq, "session", nack.SessionID)
       go func(n protocol.ControlNACK) {
           if cc.onNACK != nil {
               cc.onNACK(n.MissingSeq, n.SessionID)
           }
       }(nack)
   ```

4. **Novo método `SendRetransmitResult`:**
   ```go
   func (cc *ControlChannel) SendRetransmitResult(missingSeq uint32, success bool) error
   ```

#### [MODIFY] [backup.go](file:///home/g0004218/Projects/n-backup/internal/agent/backup.go)

Em `runParallelBackup`, após criar o Dispatcher, registrar o NACK handler:
```go
controlCh.SetOnNACK(func(missingSeq uint32, sessionID string) {
    if sessionID != dispatcher.sessionID { return }
    ok, err := dispatcher.RetransmitChunk(missingSeq)
    if err != nil {
        logger.Error("retransmit failed", "seq", missingSeq, "error", err)
    }
    controlCh.SendRetransmitResult(missingSeq, ok)
    if !ok {
        logger.Error("chunk irrecoverable, aborting session", "seq", missingSeq)
        cancel() // aborta o contexto do backup
    }
})
```

---

### Configuration

#### [MODIFY] [server.go](file:///home/g0004218/Projects/n-backup/internal/config/server.go)

Nova seção na config do server:
```go
type GapDetectionConfig struct {
    Enabled          bool          `yaml:"enabled"`           // default: true
    Timeout          time.Duration `yaml:"timeout"`           // default: 60s (2x streamReadDeadline)
    CheckInterval    time.Duration `yaml:"check_interval"`    // default: 5s
    MaxNACKsPerCycle int           `yaml:"max_nacks_per_cycle"` // default: 5
}
```

#### [MODIFY] [server.example.yaml](file:///home/g0004218/Projects/n-backup/configs/server.example.yaml)

```yaml
gap_detection:
  enabled: true
  timeout: 60s
  check_interval: 5s
  max_nacks_per_cycle: 5
```

---

## Verification Plan

### Automated Tests

**Testes unitários:**
```bash
go test -v -run TestGapTracker ./internal/server/...
go test -v -run TestControlNACK ./internal/protocol/...
go test -v -run TestChunkMap ./internal/agent/...
go test -v -run TestRetransmitChunk ./internal/agent/...
```

**Teste E2E — New scenario `TestEndToEnd_ParallelGapRetransmission`:**
```bash
go test -v -timeout 60s -run TestEndToEnd_ParallelGapRetransmission ./internal/integration/...
```

Este teste simulará:
1. Inicia server + client com 2 streams (256KB chunks)
2. Envia chunks seq 0,1,2 normalmente pelo stream 0
3. **Simula gap**: envia chunk seq 4 (skipando seq 3) pelo stream 0
4. Estabelece control channel
5. Espera server enviar `ControlNACK(missingSeq=3)`
6. Client envia chunk 3 como retransmissão
7. Client envia `ControlRetransmitResult(missingSeq=3, success=true)`
8. Continua fluxo normal → Trailer → FinalACK → backup válido

**Build completo:**
```bash
go build ./...
```

**Testes existentes (regressão):**
```bash
go test -v -timeout 120s ./internal/integration/...
go test -v ./internal/server/...
go test -v ./internal/protocol/...
go test -v ./internal/agent/...
```

### Manual Verification

Não necessária nesta fase — o teste E2E cobre o cenário crítico. O cenário de abort (ring buffer sobrescrito) pode ser adicionado como segundo teste E2E após a primeira rodada.
