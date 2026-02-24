# WebUI — Observabilidade do Chunk Buffer

## Contexto

Foi implementado um `ChunkBuffer` global no servidor (`internal/server/chunkbuffer.go`).
É um buffer em memória compartilhado entre todas as sessões paralelas.
Os dados já estão disponíveis via `ChunkBuffer.Stats()` → `ChunkBufferStats`:

```go
type ChunkBufferStats struct {
    Enabled            bool
    CapacityBytes      int64    // memória total configurada
    InFlightBytes      int64    // bytes atualmente no buffer
    FillRatio          float64  // InFlightBytes / CapacityBytes (0.0–1.0)
    TotalPushed        int64    // chunks aceitos no buffer (acumulado)
    TotalDrained       int64    // chunks escritos no disco pelo drainer (acumulado)
    TotalFallbacks     int64    // chunks enviados direto ao assembler (chunk > espaço)
    BackpressureEvents int64    // vezes que Push() bloqueou por timeout
    DrainRatio         float64  // limiar configurado (0.0 = write-through)
}
```

O buffer também rastreia bytes por sessão (`sessionBytes sync.Map: *ChunkAssembler → *atomic.Int64`).
Esse dado **ainda não está exposto via API** — precisa ser incluído no response de sessão.

O Handler já possui `h.chunkBuffer *ChunkBuffer`. Verificar se `ChunkBufferStats` já está em
`MetricsSnapshot()` em `handler.go`; se não, incluir.

---

## Backend — novas métricas a expor

### 1. Taxa de drain (bytes/s)

Ainda não existe. Adicionar em `ChunkBuffer`:
- `drainedBytesTotal atomic.Int64` — atualizado em `drainSlot()`.
- Cálculo de `bytes drenados / segundo` como média simples (janela 5s).
  Pode ser calculado on-demand em `Stats()` com snapshot periódico via timestamp.

### 2. Bytes da sessão no buffer

`ChunkBuffer.SessionBytes(assembler *ChunkAssembler) int64` — já calculável via
`sessionBytes.Load(assembler)`. Expor no endpoint `/api/v1/sessions/{id}`.

Adicionar ao `SessionDetail` (em `observability/`):

```go
BufferInFlightBytes  int64   // bytes desta sessão ainda no chunk buffer
BufferFillPercent    float64 // BufferInFlightBytes / CapacityBytes * 100
```

---

## Frontend — novos elementos visuais

### Card global "Chunk Buffer" (página principal / server stats)

Exibir somente quando `Enabled == true`.

```
┌─────────────────────────────────────────────┐
│  CHUNK BUFFER                    [● ATIVO]  │
│                                             │
│  Capacidade   64 MB                         │
│  Em uso       12.4 MB  ████░░░░░ 19.4%      │  ← gauge (verde/amarelo/vermelho)
│  Drain ratio  0.5                            │
│                                             │
│  Drenagem     18.2 MB/s   (taxa atual)       │
│  Fallbacks    3           (acumulado)        │
│  Backpressure 0           (acumulado)        │
│  Pendentes    248         (pushed - drained) │
└─────────────────────────────────────────────┘
```

Regras do gauge de fill:
- Verde  ≤ 50%
- Amarelo 50–80%
- Vermelho > 80%

Backpressure > 0 → badge vermelho pulsante.

Se `Enabled == false` → card cinza com texto "Buffer desabilitado (`size: 0`)."

### Linha extra nas sessões ativas

Abaixo do throughput de rede de cada sessão paralela ativa:

```
Buffer   [███░░░] 3.1 MB (4.8% do buffer total)   Drain: 8.7 MB/s
```

- Barra proporcional ao `BufferFillPercent` da sessão.
- Só aparece se buffer habilitado e `BufferInFlightBytes > 0`.

### Seção no detalhe de sessão (`/sessions/{id}`)

Adicionar bloco "Buffer de Memória":

| Métrica | Valor |
|---|---|
| Bytes no buffer | 3.1 MB |
| % do buffer total | 4.8% |
| Taxa de drain | 8.7 MB/s |

---

## Dicas de implementação

- Polling da SPA já ocorre a cada 2s — calcular `drain_rate` no backend com janela de 5s.
- Para drain rate: salvar `{timestamp, drainedBytesTotal}` no último `Stats()` call e calcular `Δbytes / Δt`.
- Usar `formatBytes()` existente no frontend para todos os valores de bytes.
- Reutilizar o componente de gauge já existente para throughput de streams (ver `observability/static/`).
- Backpressure é contador acumulado — deixar claro no tooltip: "total desde o início do servidor".

---

## Arquivos-chave para investigar

| Arquivo | O que encontrar |
|---|---|
| `internal/server/chunkbuffer.go` | `ChunkBuffer`, `ChunkBufferStats`, `Stats()`, `drainSlot()` |
| `internal/server/handler.go` | `MetricsSnapshot()`, `SessionDetail()`, `h.chunkBuffer` |
| `internal/server/observability/` | structs de response, router, handlers HTTP |
| `internal/server/observability/static/` | SPA JS/HTML — componentes de gauge e card existentes |
