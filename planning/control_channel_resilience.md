# Canal de Controle Persistente + Resiliência de Streaming Paralelo

## Contexto

Incidente em produção (2026-02-16 ~03:18Z) revelou que o flow rotation — implementado no [v1.3.5](file:///home/lucas/Projects/n-backup/planning/v1.3.5_offset_fix.md) — pode causar **perda de dados em trânsito** durante reconexões, resultando em dessincronização do stream (dados corrompidos interpretados como ChunkHeaders, ex: `invalid length 3013847643`).

O [v1.3.5_offset_fix.md](file:///home/lucas/Projects/n-backup/planning/v1.3.5_offset_fix.md) corrigiu o desync **estático** (contabilização de `ChunkHeaderSize`), mas existe um desync **dinâmico** causado pela janela de dados em trânsito TCP + RingBuffer circular avançando via `Advance()`.

### Root Cause

1. Agent escreve chunk no TCP → `sendOffset` avança
2. Flow rotation fecha conn → dados em trânsito se perdem
3. Server reporta `lastOffset` (menor que `sendOffset` do agent)
4. Agent faz `ReadAt(lastOffset)` no RingBuffer — se `Advance()` já liberou essa região, `ReadAt` retorna `ErrOffsetExpired` ou dados incorretos (wrap-around)
5. Server parseia garbage como ChunkHeader → `invalid length` → loop infinito de retries

### Evolução Proposta

Implementação em **3 fases progressivas**, cada uma adicionando uma camada de resiliência.

---

## Fase 1 — Fix Imediato: Validação de Resume + ErrOffsetExpired (v1.4.0)

Fix mínimo e seguro para prevenir o loop infinito de dessincronização.

### Componente: Agent Dispatcher

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

**1. Tratar `ErrOffsetExpired` no sender** (linhas 265-272):
- Quando `ReadAt` retorna `ErrOffsetExpired`, o stream deve entrar em reconnect (não morrer silenciosamente)
- Log explícito: "ring buffer offset expired, data was already freed — stream needs full reset"
- Marcar stream como `dead` e desativar — é irrecuperável sem reenviar do início

**2. Validar alinhamento após resume** (após linha 317):
- Após setar `sendOffset = resumeOffset`, ler os primeiros `ChunkHeaderSize` bytes do RB na posição `resumeOffset`
- Verificar que os bytes formam um ChunkHeader válido (`Length ≤ chunkSize` e `GlobalSeq` razoável)
- Se inválido → log de erro explícito e marcar stream como dead (evita loop de garbage)

**3. Proteger contra `Advance` para posição intermediária**:
- Após reconnect, verificar `rb.Contains(resumeOffset)` antes de tentar `ReadAt`
- Se `!Contains` → log e dead imediatamente, sem retry infinito

### Componente: RingBuffer

#### [MODIFY] [ringbuffer.go](file:///home/lucas/Projects/n-backup/internal/agent/ringbuffer.go)

**Adicionar `ContainsRange(offset, length)`**:
- Verifica se uma faixa completa de bytes está disponível no buffer
- Usado pelo sender para validar antes de ler

---

## Fase 2 — Canal de Controle Persistente (v1.5.0)

Conexão TLS persistente entre agent e server para keep-alive, RTT contínuo e pre-flight check.

### Componente: Config Agent

#### [MODIFY] [agent.go](file:///home/lucas/Projects/n-backup/internal/config/agent.go)

Expandir `DaemonInfo` (atualmente vazia) com configurações do canal de controle:

```yaml
daemon:
  control_channel:
    enabled: true               # default: true
    keepalive_interval: 30s     # intervalo do PING/PONG
    reconnect_delay: 5s         # delay antes de reconectar
    max_reconnect_delay: 5m     # backoff máximo
```

### Componente: Protocol

#### [NEW] [control.go](file:///home/lucas/Projects/n-backup/internal/protocol/control.go)

Novos frames para o canal de controle:

| Frame | Direção | Tamanho | Campos |
|-------|---------|---------|--------|
| `ControlPing` | Agent → Server | 12B | Magic(4B) `CPNG` + Timestamp(8B) |
| `ControlPong` | Server → Agent | 20B | Magic(4B) `CPNG` + Timestamp(8B) + ServerLoad(4B) + DiskFree(4B) |

### Componente: Agent Control Channel

#### [NEW] [control_channel.go](file:///home/lucas/Projects/n-backup/internal/agent/control_channel.go)

`ControlChannel` — gerencia a conexão persistente com o server:

```
State Machine:
  DISCONNECTED → CONNECTING → CONNECTED → DEGRADED → DISCONNECTED
```

**Responsabilidades**:
- Manter conn TLS persistente com o server (magic `CTRL` no handshake)
- Enviar `ControlPing` a cada `keepalive_interval`
- Calcular RTT running average (EWMA, α=0.25)
- Detectar desconexão (3 pings sem pong = DEGRADED → DISCONNECTED)
- Reconectar com exponential backoff
- Expor `IsConnected() bool`, `RTT() time.Duration`, `ServerLoad() float64`

### Componente: Agent Daemon

#### [MODIFY] [daemon.go](file:///home/lucas/Projects/n-backup/internal/agent/daemon.go)

- Iniciar `ControlChannel` antes do Scheduler
- Injetar referência do `ControlChannel` no Scheduler para pre-flight check
- Lifecycle: `ControlChannel.Start()` no boot, `ControlChannel.Stop()` no shutdown
- Reload via SIGHUP: parar e recriar o canal se o server address mudou

### Componente: Agent Scheduler

#### [MODIFY] [scheduler.go](file:///home/lucas/Projects/n-backup/internal/agent/scheduler.go)

- Pre-flight check antes de disparar backup: `if !controlChannel.IsConnected() { skip }`
- Log: "skipping scheduled backup: server unreachable"
- RTT disponível para o dispatcher via `controlChannel.RTT()`

### Componente: Server Handler

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- Novo magic `CTRL` no `HandleConnection`
- Handler `handleControlChannel`: loop de `ControlPing` → `ControlPong` com server load metrics
- Goroutine leve — uma por agent conectado

---

## Fase 3 — Flow Rotation Graceful + Orquestração (v1.6.0)

> [!IMPORTANT]
> Fase 3 depende da Fase 2 (canal de controle) estar funcional.

### Evolução do Flow Rotation

Em vez de fechar a conn abruptamente, o server negocia a rotação pelo canal de controle:

```
Server                              Agent
  │                                   │
  │── ControlRotate(streamIdx) ──────>│  (via control channel)
  │                                   │── drena chunks pendentes
  │                                   │── confirma SACKs pendentes
  │<── ControlRotateACK(streamIdx) ──│
  │                                   │
  │   (fecha conn de dados)           │── reconnect stream
```

**Benefício**: Zero dados perdidos. O agent sabe que vai rotacionar e drena tudo antes.

### Orquestração Futura (Admission Control)

O canal de controle permite o server orquestrar backups:

| Frame | Direção | Uso |
|-------|---------|-----|
| `ControlAdmit` | Server → Agent | "Pode iniciar backup no slot X" |
| `ControlDefer` | Server → Agent | "Espere N minutos, estou ocupado" |
| `ControlAbort` | Server → Agent | "Disco cheio, aborte" |

> [!NOTE]
> Fase 3 é mais complexa e deve ser planejada em detalhe após as Fases 1 e 2 estarem em produção.

---

## Verificação

### Fase 1 — Testes Automatizados

```bash
# Testes unitários existentes do RingBuffer (devem continuar passando)
go test ./internal/agent/ -run TestRingBuffer -v

# Testes do Dispatcher existentes
go test ./internal/agent/ -run TestDispatcher -v

# Todos os testes do projeto
go test ./...
```

**Novos testes a adicionar:**
- `TestRingBuffer_ContainsRange` — validar novo método
- `TestSender_ErrOffsetExpired_MarksStreamDead` — simular `Advance` além do `sendOffset` e verificar que o stream é desativado (não entra em loop)
- `TestSender_ResumeValidation_InvalidHeader` — simular resume com dados corrompidos no offset e verificar que o stream é marcado como dead

### Fase 2 — Testes Automatizados + Manual

```bash
# Novos testes unitários do control channel
go test ./internal/agent/ -run TestControlChannel -v

# Testes do protocol de controle
go test ./internal/protocol/ -run TestControl -v

# Build completo
go test ./...
```

**Verificação manual (produção)**:
- Iniciar agent daemon e verificar nos logs que o canal de controle conectou
- Parar o server e verificar que o agent detecta desconexão e loga "server unreachable"
- Reiniciar o server e verificar que o canal reconecta automaticamente
- Disparar backup com `--once` e verificar que o RTT é reportado nos logs
