# Fase 3 — Flow Rotation Graceful + Orquestração (v1.6.0)

O flow rotation atual (Fase 1) fecha conexões abruptamente quando detecta throughput degradado. Isso causa perda de dados em trânsito e obriga o agent a fazer retry com backoff + resume.

A Fase 3 evolui o canal de controle (Fase 2 — PING/PONG unidirecional) para full-duplex, permitindo ao server negociar gracefully a rotação de streams pelo canal de controle antes de fechar a conexão de dados.

## User Review Required

> [!IMPORTANT]
> O canal de controle atual usa um modelo request-response síncrono (agent envia PING, espera PONG). Para o server enviar `ControlRotate` assincronamente, precisamos refatorar o `pingLoop` do agent para full-duplex:
> - **Writer goroutine**: envia PINGs periódicos
> - **Reader goroutine**: lê qualquer frame (PONG, ControlRotate) e despacha
>
> Isso muda o design do `ControlChannel` no agent. A alternativa seria manter o model request-response e o server envia `ControlRotate` como resposta ao próximo PING, mas isso introduz latência de até `keepalive_interval` (30s) — inaceitável para flow rotation.

> [!IMPORTANT]
> Os frames de **Admission Control** (`ControlAdmit`, `ControlDefer`, `ControlAbort`) estão no escopo do plano original mas são "orquestração futura". Proponho implementar apenas a **infraestrutura** (definir os frames no protocol) e **não** a lógica de consumo no agent/server nesta versão — fica como extensão da v1.7.0.

## Proposed Changes

### Protocol Layer

#### [MODIFY] [control.go](file:///home/lucas/Projects/n-backup/internal/protocol/control.go)

Adicionar novos frames para negociação de rotation e orquestração futura:

**Frames de Rotation:**

| Frame | Magic | Direção | Tamanho | Campos |
|-------|-------|---------|---------|--------|
| `ControlRotate` | `CROT` | Server → Agent | 5B | Magic(4B) + StreamIndex(1B) |
| `ControlRotateACK` | `CRAK` | Agent → Server | 5B | Magic(4B) + StreamIndex(1B) |

O design é intencionalmente simples: 1 frame = 1 stream. O server envia um `ControlRotate` por stream e espera o ACK correspondente.

**Frames de Orquestração (apenas definição, sem consumo):**

| Frame | Magic | Direção | Tamanho | Campos |
|-------|-------|---------|---------|--------|
| `ControlAdmit` | `CADM` | Server → Agent | 5B | Magic(4B) + SlotID(1B) |
| `ControlDefer` | `CDFE` | Server → Agent | 8B | Magic(4B) + WaitMinutes(4B) |
| `ControlAbort` | `CABT` | Server → Agent | 8B | Magic(4B) + Reason(4B) |

Funções `Write*`/`Read*` para todos os frames. Round-trip tests.

---

#### [MODIFY] [control_test.go](file:///home/lucas/Projects/n-backup/internal/protocol/control_test.go)

Adicionar testes round-trip para cada novo frame.

---

### Server Handler

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

**1. Registrar control channel conn por agent:**

Adicionar no `Handler`:
```go
controlConns sync.Map // agentName (string) → net.Conn
```

Em `handleControlChannel`, após identificar o agent (extrair do TLS peer certificate CN), registrar a conn:
```go
h.controlConns.Store(agentName, conn)
defer h.controlConns.Delete(agentName)
```

**2. Refatorar `handleControlChannel` para full-duplex:**

O handler de control channel atual é síncrono (lê PING → responde PONG). Precisa ler frames assíncronos do agent também (`ControlRotateACK`). Refatorar para:
- **Reader goroutine**: lê frames do agent (PING ou ControlRotateACK) e despacha
- PING → responde PONG diretamente
- ControlRotateACK → sinaliza canal de pending rotations

Adicionar no `ParallelSession`:
```go
RotatePending sync.Map // streamIndex (uint8) → chan struct{} (sinalizada por ControlRotateACK)
```

**3. Refatorar `evaluateFlowRotation`:**

Quando flow rotation é necessário:
1. Busca control channel conn do agent via `h.controlConns.Load(ps.AgentName)`
2. Se control channel disponível: envia `ControlRotate(streamIdx)` → cria canal em `RotatePending` → espera ACK com timeout (10s)
3. Após ACK: fecha conn de dados (o agent já drenou)
4. Se control channel **não** disponível ou timeout: fallback para comportamento atual (fechar conn imediatamente)

---

#### [MODIFY] [handler_flow_rotation_test.go](file:///home/lucas/Projects/n-backup/internal/server/handler_flow_rotation_test.go)

Adicionar testes:
- `TestEvaluateFlowRotation_GracefulWithControlChannel` — verifica que o server envia `ControlRotate` pelo control channel antes de fechar a conn
- `TestEvaluateFlowRotation_FallbackWithoutControlChannel` — confirma que sem control channel, o comportamento atual (fechar conn) é preservado
- `TestEvaluateFlowRotation_FallbackOnACKTimeout` — timeout do ACK resulta em close abrupto

---

### Agent Control Channel

#### [MODIFY] [control_channel.go](file:///home/lucas/Projects/n-backup/internal/agent/control_channel.go)

**1. Refatorar `pingLoop` para full-duplex:**

Separar em duas goroutines:
- `pingWriter()`: envia ControlPing periódico (ticker)
- `frameReader()`: lê qualquer frame do server (ControlPong, ControlRotate)

Despacho no reader:
```go
magic := readMagic(conn)
switch magic:
  case "CPNG" → parse ControlPong → updateRTT, métricas
  case "CROT" → parse ControlRotate → chamar rotateHandler
```

**2. Adicionar callback de rotation:**

```go
type ControlChannel struct {
    // ... campos existentes ...
    onRotate func(streamIndex uint8) // callback para notificar dispatcher
}
```

O callback é chamado quando `ControlRotate` é recebido. Após o dispatcher confirmar drain, envia `ControlRotateACK`.

**3. Expor `SendRotateACK(streamIndex uint8)`**:

Escreve `ControlRotateACK` na conn do control channel (com mutex de write).

---

### Agent Dispatcher

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

**1. Adicionar `DrainStream(streamIdx int) error`:**

- Pausa o envio de **novos** chunks para este stream (marca como `draining`)
- Espera os chunks pendentes no pipe TCP serem confirmados (via SACK do server na conn de dados)
- Timeout de 10s para drain
- Não precisa esperar o ring buffer esvaziar — apenas os dados já escritos no TCP

**2. Adicionar campo `draining` no `ParallelStream`:**

```go
type ParallelStream struct {
    // ... existentes ...
    draining atomic.Bool  // true = não aceita novos chunks, está drenando
}
```

**3. Modificar `emitChunk()`:**

Ao selecionar próximo stream em round-robin, pular streams com `draining=true` (igual a `dead`).

**4. Integrar com ControlChannel:**

No `daemon.go`, ao criar o ControlChannel, setar o callback `onRotate` para chamar:
1. `dispatcher.DrainStream(streamIdx)`
2. `controlChannel.SendRotateACK(streamIdx)`

> [!NOTE]
> O dispatcher pode não existir quando o rotate chega (pode não haver backup ativo). O callback deve verificar se há dispatcher ativo.

---

### Agent Daemon

#### [MODIFY] [daemon.go](file:///home/lucas/Projects/n-backup/internal/agent/daemon.go)

- Configurar o `onRotate` callback no ControlChannel
- Como o dispatcher é transitório (criado por backup), o callback precisa de uma referência indireta — usar um `sync.Mutex`-protegido pointer para o dispatcher ativo

---

## Verification Plan

### Automated Tests

```bash
# 1. Testes dos novos frames de protocol (round-trip)
go test ./internal/protocol/ -run TestControl -v

# 2. Testes existentes de flow rotation (devem continuar passando)
go test ./internal/server/ -run TestEvaluateFlowRotation -v

# 3. Novos testes de flow rotation graceful
go test ./internal/server/ -run TestEvaluateFlowRotation_Graceful -v
go test ./internal/server/ -run TestEvaluateFlowRotation_Fallback -v

# 4. Testes dos novos frames de controle
go test ./internal/protocol/ -run TestControlRotate -v

# 5. Suite completa
go test ./...
```

### Manual Verification

> [!NOTE]
> Verificação manual requer ambiente de produção com agent e server. Proponho incluir instruções no walkthrough mas deixar a validação real para deploy. Os testes automatizados cobrem a lógica core.
