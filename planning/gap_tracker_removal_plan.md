# Remoção do GapTracker e Cadeia NACK/Retransmit

Remoção completa do `GapTracker`, `ControlNACK`, `ControlRetransmitResult` e config `gap_detection`. O mecanismo de `ChunkSACK` per-chunk + reconnect via re-join já cobre o cenário de detecção e recuperação de chunks perdidos.

> [!IMPORTANT]
> A seção `gap_detection` no YAML será **preservada na struct** mas ignorada. Se presente no YAML, o server emite `WARN` no startup informando que a feature foi descontinuada.

---

## Proposed Changes

### Componente 1: Server — GapTracker (delete + limpeza)

---

#### [DELETE] [gap_tracker.go](file:///home/lucas/Projects/n-backup/internal/server/gap_tracker.go)

Arquivo inteiro deletado (~353 linhas).

#### [DELETE] [gap_tracker_test.go](file:///home/lucas/Projects/n-backup/internal/server/gap_tracker_test.go)

Arquivo inteiro deletado.

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- **Remover** campo `GapTracker *GapTracker` da `ParallelSession` (L2201)
- **Remover** `gapCheckLoop` inteiro (L1468-L1538)
- **Remover** criação do `GapTracker` em `handleParallelBackup` (L2314-L2322)
- **Remover** inicialização do gap check loop (L2355-L2359)
- **Remover** comentários sobre `GapTracker`/`gapCheckLoop` espalhados
- **Remover** todos os blocos `if session.GapTracker != nil { ... }` em `receiveParallelStream` (L2566-L2567, L2581-L2582, L2586-L2587, L2593-L2594, L2599-L2600, L2604-L2605)
- **Remover** `if session.GapTracker != nil { ... }` em `readParallelChunkPayload` (L2505-L2506)
- **Remover** comentários sobre `inFlightTimeout`/`GapTracker` em `readParallelChunkPayload` (L2495-L2496)
- **Remover** `RearmGap` no handler de `ControlRetransmitResult` (L1378-L1379)
- **Remover** handler de `MagicControlRetransmitResult` inteiro no control channel (L1346-L1392)
- **Remover** handler de `MagicControlNACK` + todo o `gapCheckLoop` (L1463-L1537)

**Enriquecer logging de lifecycle de chunk** no `receiveParallelStream`:
- Onde antes havia `GapTracker.StartChunk` → manter o log `chunk_receive_started` (já existe L2559-L2564)
- Onde antes havia `GapTracker.CompleteChunk` → log `chunk_receive_completed` já existe como `chunk_received` (L2618-L2623)
- Onde havia `GapTracker.AbandonChunk` → adicionar log **explícito** `chunk_receive_failed` com `globalSeq`, `stream`, `error` nos blocos de erro do buffer push e assembler write
- Onde havia `MarkBuffered` → adicionar log `chunk_buffered` com o `globalSeq`

#### [MODIFY] [slot.go](file:///home/lucas/Projects/n-backup/internal/server/slot.go)

- Atualizar comentário do `ChunksLost` (L88) — remover referência ao `GapTracker`

---

### Componente 2: Protocol — ControlNACK e ControlRetransmitResult

---

#### [MODIFY] [control.go](file:///home/lucas/Projects/n-backup/internal/protocol/control.go)

- **Remover** `MagicControlNACK` (L49-L51)
- **Remover** `MagicControlRetransmitResult` (L53-L55)
- **Remover** `ControlNACK` struct (L160-L166)
- **Remover** `ControlRetransmitResult` struct (L167-L175)
- **Remover** `WriteControlNACK` (L558-L575)
- **Remover** `ReadControlNACKPayload` (L573-L595)
- **Remover** `WriteControlRetransmitResult` (L599-L615)
- **Remover** `ReadControlRetransmitResultPayload` (L617-L637)

#### [MODIFY] [control_test.go](file:///home/lucas/Projects/n-backup/internal/protocol/control_test.go)

- **Remover** `TestControlRetransmitResult_RoundTrip` (L251)
- **Remover** `TestControlRetransmitResult_SessionIDTooLong` (L275)
- **Remover** testes de `ControlNACK` (se existirem)

---

### Componente 3: Agent — NACK handler e RetransmitResult

---

#### [MODIFY] [control_channel.go](file:///home/lucas/Projects/n-backup/internal/agent/control_channel.go)

- **Remover** campo `onNACK func(...)` (L68-L70)
- **Remover** `SetOnNACK` (L107-L111)
- **Remover** `SendRetransmitResult` (L217-L237)
- **Remover** case `protocol.MagicControlNACK` no frame reader (L575-L601)

#### [MODIFY] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

- **Remover** bloco `controlCh.SetOnNACK(...)` inteiro (L559-L584)

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

- **Manter** `RetransmitChunk` e o `chunkRecord` ring buffer — eles são usados internamente pelo dispatcher para per-N-chunk rotation e reconexão. A lógica de retransmissão local persiste, apenas o gatilho remoto (NACK do server) é desligado.

#### [MODIFY] [dispatcher_test.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher_test.go)

- **Manter** `TestDispatcher_RetransmitChunk_UsesOriginalStream` — a funcionalidade ainda existe, apenas não é acionada por NACK externo.

---

### Componente 4: Config — Deprecação graceful

---

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

- **Manter** `GapDetectionConfig` struct e `UnmarshalYAML` para backward compatibility de YAML
- **Remover** defaults do `validate()` para `GapDetection` (L313-L330) — não aplicar mais defaults
- **Adicionar** método `WarnDeprecated(logger)` no `ServerConfig` que emite:
  ```
  WARN: gap_detection configuration is deprecated and will be ignored.
        ChunkSACK per-chunk acknowledgment replaces gap detection since v3.0.0.
  ```
  Chamado no `main.go` do server após `LoadServerConfig`, quando `GapDetection.enabledSet == true` (ou seja, o YAML tem a seção explicitamente).

#### [MODIFY] [config_test.go](file:///home/lucas/Projects/n-backup/internal/config/config_test.go)

- **Remover** `TestLoadServerConfig_GapDetectionDefaultsEnabled` (L447)
- **Remover** `TestLoadServerConfig_GapDetectionExplicitlyDisabled` (L482)
- **Adicionar** `TestLoadServerConfig_GapDetectionDeprecatedIgnored` — verifica que a config parseia sem erro mesmo com `gap_detection` presente

---

### Componente 5: Docs e Configs de Exemplo

---

#### [MODIFY] [server.example.yaml](file:///home/lucas/Projects/n-backup/configs/server.example.yaml)

- **Commentar** seção `gap_detection` com nota de deprecação:
  ```yaml
  # DEPRECATED: gap_detection was removed in v3.0.0.
  # ChunkSACK per-chunk acknowledgment replaces this functionality.
  # gap_detection:
  #   enabled: true
  ```

#### [MODIFY] [usage.md](file:///home/lucas/Projects/n-backup/docs/usage.md)

- Marcar seção `gap_detection` como **deprecated**

#### [MODIFY] [Configuracao-de-Exemplo.md](file:///home/lucas/Projects/n-backup/wiki/Configuracao-de-Exemplo.md)

- Marcar seção `gap_detection` como **deprecated**

#### [MODIFY] [Guia-de-Uso.md](file:///home/lucas/Projects/n-backup/wiki/Guia-de-Uso.md)

- Marcar seção `gap_detection` como **deprecated**

#### [MODIFY] [CHANGELOG.md](file:///home/lucas/Projects/n-backup/CHANGELOG.md)

- Adicionar entrada para remoção do GapTracker

---

### Componente 6: Handler Flow Rotation Tests

---

#### [MODIFY] [handler_flow_rotation_test.go](file:///home/lucas/Projects/n-backup/internal/server/handler_flow_rotation_test.go)

- Remover referências ao `GapTracker` se existirem na criação de `ParallelSession` em testes

---

## Verification Plan

### Testes Automatizados

```bash
# Build completo (verifica que todas as referências foram removidas)
go build ./cmd/...

# Testes de protocolo (ControlSlotPark/Resume persistem, NACK/Retransmit removidos)
go test -race -v ./internal/protocol/

# Testes de config (novo teste de deprecação)
go test -race -v ./internal/config/ -run TestLoadServerConfig

# Testes de server (flow rotation, assembler, chunkbuffer — sem gap tracker)
go test -race -v ./internal/server/

# Testes de agent (dispatcher, control channel, backup)
go test -race -v ./internal/agent/

# Suite completa com race detector
go test -race ./...
```

### Verificação Manual

1. **Config YAML existente**: Iniciar server com YAML que contém `gap_detection: enabled: true` e verificar que um `WARN` aparece nos logs e o server inicia normalmente.
