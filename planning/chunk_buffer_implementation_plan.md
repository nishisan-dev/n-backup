# Chunk Buffer Global — Plano de Implementação

## Objetivo

Introduzir um **buffer de chunks em memória pré-alocado e global** por instância de servidor.
O buffer atua como um *damper* de I/O: os chunks recebidos da rede são primeiro empurrados para o buffer e, em paralelo, um processo de drenagem os escreve no disco, mantendo um fluxo mais uniforme e reduzindo o impacto de oscilações em HDs lentos.

### Comportamento resumido

| Parâmetro | Valor | Semântica |
|---|---|---|
| `chunk_buffer_size: 0` | desligado | comportamento atual, sem alteração |
| `chunk_buffer_size: 64mb` + `chunk_buffer_drain: lazy` | habilitado, lazy | chunks vão para o buffer → goroutine de drenagem lê do buffer e chama `WriteChunk` no assembler |
| `chunk_buffer_size: 64mb` + `chunk_buffer_drain: eager` | habilitado, eager | chunks vão para o buffer → a goroutine de drenagem faz append imediato no arquivo final (sem reordenação — presume chegada in-order ou aplica a ordem da fila) |

> [!IMPORTANT]
> O buffer é **global por server**, não por sessão. Uma única instância de `ChunkBuffer` é criada no startup e compartilhada. O buffer controla backpressure via semáforo de slots.
>
> O **drain mode** é `lazy` por padrão (usa o caminho normal do assembler) ou `eager` (drain direto append-only, ignora o assembler existente — adequado apenas para streams já ordenados).

---

## Proposed Changes

### Config (`internal/config/server.go`)

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

Adicionar nova struct e campo a `ServerConfig`:

```go
// ChunkBufferConfig define o buffer de chunks em memória compartilhado entre sessões.
type ChunkBufferConfig struct {
    Size      string `yaml:"size"`       // "0" desliga; ex: "64mb", "256mb"
    DrainMode string `yaml:"drain_mode"` // "lazy" (default) | "eager"
    SizeRaw   int64  `yaml:"-"`          // parsed por validate()
}
```

Adicionar campo `ChunkBuffer ChunkBufferConfig` em `ServerConfig`.

Em `validate()`:
- Se `Size == ""` ou `Size == "0"` → `SizeRaw = 0` (desligado).
- Parsear com o `ParseByteSize` existente.
- `DrainMode` default: `"lazy"`. Aceitar `"eager"`.

---

### Chunk Buffer (`internal/server/chunkbuffer.go`) — arquivo **novo**

#### [NEW] [chunkbuffer.go](file:///home/lucas/Projects/n-backup/internal/server/chunkbuffer.go)

Struct principal:

```go
// ChunkBuffer é um buffer em memória global que absorve chunks recebidos e
// os drena para o assembler de forma assíncrona, suavizando picos de I/O.
type ChunkBuffer struct {
    slots     chan chunkSlot  // canal com capacidade = número de slots
    drainMode string          // "lazy" | "eager"
    logger    *slog.Logger
}

// chunkSlot representa um chunk no buffer.
type chunkSlot struct {
    sessionID string
    globalSeq uint32
    data      []byte       // cópia dos dados do chunk
    assembler *ChunkAssembler  // referência ao assembler destino
}
```

Capacidade de slots: `SizeRaw / avgChunkSize` onde `avgChunkSize` é uma constante interna (default 1 MB — pode ser ajustada). O buffer aloca a memória ao inicializar através da capacidade do canal (Go não pré-aloca sem escrita, mas a capacidade limita o uso máximo).

**Métodos:**
- `NewChunkBuffer(cfg ChunkBufferConfig, logger *slog.Logger) *ChunkBuffer` — retorna `nil` quando desligado.
- `Push(slot chunkSlot) error` — tenta enviar ao canal com timeout; retorna erro se cheio por muito tempo (backpressure).
- `StartDrainer(ctx context.Context)` — inicia goroutine de drenagem única.
- `Stats() ChunkBufferStats` — métricas atômicas: `Capacity`, `InFlight`, `TotalPushed`, `TotalDrained`, `BackpressureEvents`.

**Drainer loop (lazy drain):**
```
for slot := range slots:
    slot.assembler.WriteChunk(slot.globalSeq, bytes.NewReader(slot.data), len(slot.data))
```

**Drainer loop (eager drain):**
Ordem chegada → append direto no `outFile` do assembler via método novo `AppendDirect(data []byte)`. Adequado para fluxos já sequenciais (single stream ou round-robin perfeito).

> [!WARNING]
> No modo `eager`, o assembler não faz reordenação — é responsabilidade do caller garantir que os chunks cheguem na ordem correta para o eager drain funcionar corretamente. Recomendado apenas para cenários single-stream ou quando o buffer absorve os chunks e os ordena antes de drená-los.
> 
> Para evitar complexidade excessiva na v1 do buffer, o **eager drain** ainda passará os chunks pelo método `WriteChunk` do assembler, mas com o assembler já configurado em modo eager. A diferença real é que o slot no buffer permite que a goroutine de rede retorne mais rápido, deixando o I/O de disco para o drainer.

---

### Assembler (`internal/server/assembler.go`)

#### [MODIFY] [assembler.go](file:///home/lucas/Projects/n-backup/internal/server/assembler.go)

Sem alterações estruturais. O `ChunkBuffer` usa `assembler.WriteChunk(...)` normalmente.

Adicionar apenas método auxiliar de exposição do `outFile` para possível drain eager direto (fase futura), **não implementado na v1**.

---

### Handler (`internal/server/handler.go`)

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

1. Adicionar campo `chunkBuffer *ChunkBuffer` em `Handler`.
2. Em `NewHandler`, aceitar e armazenar `*ChunkBuffer` (ou construir internamente com base no cfg).
3. Criar `StartChunkBuffer(ctx context.Context)` no Handler — chama `chunkBuffer.StartDrainer(ctx)`.
4. No ponto onde um stream paralelo chama `assembler.WriteChunk(...)` (goroutine do `handleParallelJoin`):
   - Se `chunkBuffer != nil` → `chunkBuffer.Push(slot)` em vez de chamar o assembler diretamente.
   - Se `chunkBuffer == nil` → caminho atual inalterado.
5. Expor métricas do buffer via `MetricsSnapshot()` para observabilidade.

---

### Server entry point (`internal/server/server.go`)

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/server/server.go)

- Instanciar `ChunkBuffer` com base em `cfg.ChunkBuffer` no início do server.
- Chamar `handler.StartChunkBuffer(ctx)`.

---

### Configuração de exemplo

#### [MODIFY] [server.example.yaml](file:///home/lucas/Projects/n-backup/configs/server.example.yaml)

Adicionar nova seção `chunk_buffer`:

```yaml
# Buffer de chunks em memória — reduz oscilações de I/O em HDs lentos.
# 0 desliga o buffer (default). Tamanho define a memória máxima reservada.
chunk_buffer:
  size: 0            # ex: "64mb", "256mb"  (0 = desligado)
  drain_mode: lazy   # lazy (usa assembler normal) | eager (append direto)
```

---

## Verification Plan

### Testes automatizados existentes

Os testes de assembler em `internal/server/assembler_test.go` não precisam ser alterados — o assembler não muda sua API pública.

**Comandos:**
```bash
cd /home/lucas/Projects/n-backup
go test ./internal/server/ -v -run TestChunkAssembler
```

### Novos testes — `internal/server/chunkbuffer_test.go`

| Test | Descrição |
|---|---|
| `TestChunkBuffer_Disabled` | `NewChunkBuffer` com `SizeRaw=0` retorna `nil` |
| `TestChunkBuffer_Push_And_Drain_Lazy` | Push N slots → drain via goroutine → verifica ordem e conteúdo no assembler |
| `TestChunkBuffer_Push_And_Drain_Eager` | Idem para modo eager |
| `TestChunkBuffer_Backpressure` | Buffer cheio → `Push` deve respeitar backpressure (timeout ou bloqueio) |
| `TestChunkBuffer_Stats` | Verifica métricas atômicas após push/drain |

**Comandos:**
```bash
cd /home/lucas/Projects/n-backup
go test ./internal/server/ -v -run TestChunkBuffer
```

### Build limpo

```bash
cd /home/lucas/Projects/n-backup
go build ./...
```
