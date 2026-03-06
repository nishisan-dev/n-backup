# Distinguir Port Rotation de Reconnect por Erro

## Contexto

Atualmente o server trata **toda reconexão** via `ParallelJoin` como `Reconnects++` com evento level `warn`. Quando o agent usa `port_rotation.mode: "per-n-chunks"`, as reconexões intencionais (para trocar o source port TCP) são indistinguíveis das reconexões por falha de rede. Isso polui a UI com badges amarelos e eventos falsos de warning.

## Proposta

Adicionar um campo `Flags` (1 byte) ao final do frame `ParallelJoin` para sinalizar o motivo da reconexão. O server usa esse byte para separar contadores e emitir eventos com levels distintos.

> [!IMPORTANT]
> **Breaking change mínimo**: o byte de Flags é **appended** ao frame existente. Agents v3.0.x (sem o byte) continuam funcionando — o server tenta ler o byte extra; se não houver (EOF), assume `0x00` (sem flag). A leitura é feita sobre um `bufio.Reader` que já existe no `ReadParallelJoin`.

---

## Proposed Changes

### Protocolo (`internal/protocol`)

#### [MODIFY] [frames.go](file:///home/lucas/Projects/n-backup/internal/protocol/frames.go)

- Adicionar constantes de join reason:
  ```go
  // JoinReason flags para ParallelJoin.Flags.
  const (
      JoinReasonNone     byte = 0x00 // primeira conexão ou reconexão por erro
      JoinReasonRotation byte = 0x01 // reconexão intencional por port rotation
  )
  ```
- Adicionar campo `Flags byte` à struct `ParallelJoin`.

#### [MODIFY] [writer.go](file:///home/lucas/Projects/n-backup/internal/protocol/writer.go)

- Alterar `WriteParallelJoin` para aceitar `flags byte` como novo parâmetro e escrever o byte após `StreamIndex`:
  ```
  Formato: [Magic 4B][Version 1B][SessionID\n][StreamIndex 1B][Flags 1B]
  ```

#### [MODIFY] [reader.go](file:///home/lucas/Projects/n-backup/internal/protocol/reader.go)

- Alterar `ReadParallelJoin` para ler o byte de `Flags` após `StreamIndex`. Se o read retornar EOF (agent antigo sem o byte), assume `0x00`.

---

### Agent (`internal/agent`)

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

- **`reconnectStream`** (linha 780): receber parâmetro `flags byte` e passá-lo a `WriteParallelJoin`.
- **Caller no rotation** (linha 724): chamar `reconnectStream(streamIdx, protocol.JoinReasonRotation)`.
- **Caller no write error** (linha 610): chamar `reconnectStream(streamIdx, protocol.JoinReasonNone)`.
- **`ActivateStream`** (linha 893): chamar `WriteParallelJoin(... , protocol.JoinReasonNone)`.

---

### Server (`internal/server`)

#### [MODIFY] [slot.go](file:///home/lucas/Projects/n-backup/internal/server/slot.go)

- Adicionar `Rotations atomic.Int32` ao `Slot` (separado do `Reconnects` existente).

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- No `handleParallelJoin` (linhas 2582-2593): verificar `pj.Flags`:
  - Se `JoinReasonRotation`: incrementar `slot.Rotations` e emitir evento `"info"` / `"port_rotation"` ao invés de `"warn"` / `"stream_reconnect"`.
  - Se `JoinReasonNone` (ou outro): manter comportamento atual (`Reconnects++` + evento `warn`).

#### [MODIFY] [dto.go](file:///home/lucas/Projects/n-backup/internal/server/observability/dto.go)

- Adicionar `Rotations int32 \`json:"rotations"\`` ao `StreamDetail`.

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go) (build da stream detail, ~linha 592)

- Incluir `Rotations: slot.Rotations.Load()` no `StreamDetail`.

---

### Web UI (`internal/server/observability/web`)

#### [MODIFY] [components.js](file:///home/lucas/Projects/n-backup/internal/server/observability/web/js/components.js)

- Na tabela de streams (linha ~433): exibir **rotations** com badge neutro/info (azul) ao invés de warn.
- Ajustar a coluna de **reconnects** para mostrar apenas reconexões reais (sem rotations).
- Adicionar coluna (ou tooltip) de rotations se > 0.

---

## Verification Plan

### Testes Automatizados

Todos os testes devem ser executados com:
```bash
cd /home/lucas/Projects/n-backup && go test -race -count=1 ./...
```

#### Testes existentes a atualizar

1. **`TestParallelJoin_RoundTrip`** em `internal/protocol/protocol_test.go`:
   - Atualizar para passar `flags` em `WriteParallelJoin` e verificar que `ReadParallelJoin` retorna o valor correto.

2. **`TestHandleParallelJoin_RejectsClosingSessionBeforeAckOK`** em `internal/server/server_test.go`:
   - Atualizar o call a `WriteParallelJoin` para incluir o novo parâmetro `flags`.

3. **`TestReadParallelJoin_Oversized_SessionID`** em `internal/protocol/reader_limit_test.go`:
   - Verificar se precisa de ajustes (provavelmente não, pois o teste é sobre sessionID oversized).

#### Testes novos a adicionar

1. **`TestParallelJoin_RoundTrip_WithRotationFlag`** em `protocol_test.go`:
   - Escreve com `JoinReasonRotation`, lê e valida `Flags == JoinReasonRotation`.

2. **`TestParallelJoin_BackwardCompat_NoFlags`** em `protocol_test.go`:
   - Simula um agent antigo escrevendo sem o byte de Flags; `ReadParallelJoin` deve retornar `Flags == 0x00` sem erro.

### Build e Race Detector

```bash
cd /home/lucas/Projects/n-backup && go build ./...
cd /home/lucas/Projects/n-backup && go test -race -count=1 ./...
```
