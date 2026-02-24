# Bandwidth Throttling por Backup/Destino

Implementar rate limiting de upload no agent, configurável por entrada de backup (`BackupEntry`). O throttle limita a taxa de bytes/s enviados para a rede, protegendo links WAN sem desperdiçar banda.

## Proposta de Configuração YAML

```yaml
backups:
  - name: "local-backup"
    storage: "local"
    schedule: "0 2 * * *"
    # sem bandwidth_limit → full speed

  - name: "offsite-backup"
    storage: "remote-dc"
    schedule: "0 3 * * *"
    bandwidth_limit: 50mb   # 50 MB/s (usa mesma sintaxe de ParseByteSize)
    parallels: 4
```

Formato: `ParseByteSize` já existente (suporta `kb`, `mb`, `gb`). Valor em **bytes/segundo**.

## Proposed Changes

### Config

#### [MODIFY] [agent.go](file:///home/lucas/Projects/n-backup/internal/config/agent.go)

- Adicionar campo `BandwidthLimit string` e `BandwidthLimitRaw int64` em `BackupEntry`
- Validação no `validate()`: se não-vazio, parsear com `ParseByteSize`. Valor mínimo: `64kb` (para evitar configurações absurdas)

---

### Agent — Throttle Writer

#### [NEW] [throttle.go](file:///home/lucas/Projects/n-backup/internal/agent/throttle.go)

Implementar `ThrottledWriter` — um `io.Writer` wrapper com token bucket:

```go
type ThrottledWriter struct {
    w       io.Writer
    limiter *rate.Limiter // golang.org/x/time/rate
}

func NewThrottledWriter(w io.Writer, bytesPerSec int64) *ThrottledWriter
func (tw *ThrottledWriter) Write(p []byte) (int, error)
```

- Usa `rate.NewLimiter(rate.Limit(bytesPerSec), burstSize)` onde `burstSize` é o chunk natural (256KB, alinhado ao buffer de escrita existente)
- `Write()` consome tokens via `limiter.WaitN(ctx, len(p))`, depois delega para `w.Write(p)`
- Se `bytesPerSec == 0`, bypass total (noop wrapper retorna o writer original)

---

### Agent — Integração no Pipeline

#### [MODIFY] [streamer.go](file:///home/lucas/Projects/n-backup/internal/agent/streamer.go)

- Adicionar parâmetro `bandwidthLimit int64` na função `Stream()`
- Se `bandwidthLimit > 0`, envolver o `bufDest` com `NewThrottledWriter` **antes** do `countWriter`
- Pipeline resultante: `compressor → countWriter → ThrottledWriter → bufio.Writer → conn`

#### [MODIFY] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

- Passar `entry.BandwidthLimitRaw` para `Stream()` no single-stream (linha ~116)
- No parallel backup: passar o limit para o `Dispatcher` via um novo campo em `DispatcherConfig`

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

- Adicionar campo `BandwidthLimit int64` em `DispatcherConfig`
- No `startSenderWithRetry`, envolver a `conn` de cada stream com `ThrottledWriter(conn, limit / activeStreams)` — rate dividido entre streams ativos
- Alternativa mais simples: aplicar o throttle no `Write()` do Dispatcher (antes do round-robin), limitando o fluxo agregado. **Esta é a abordagem preferida** pois é um único limiter para todo o fluxo.

> [!IMPORTANT]
> Para o parallel backup, o throttle deve ser no `Write()` do `Dispatcher`, não por stream individual. Isso garante que o rate limit seja **agregado** (50 MB/s total, não 50 MB/s × N streams).

---

### Config Example e Documentação

#### [MODIFY] [agent.example.yaml](file:///home/lucas/Projects/n-backup/configs/agent.example.yaml)

- Adicionar `bandwidth_limit` no exemplo do backup `home` (ex: `bandwidth_limit: 50mb`)

#### [MODIFY] [usage.md](file:///home/lucas/Projects/n-backup/docs/usage.md)

- Documentar a opção `bandwidth_limit` na seção de configuração do agent

#### [MODIFY] [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md)

- Documentar bandwidth throttling na especificação

---

### Dependência

#### [MODIFY] [go.mod](file:///home/lucas/Projects/n-backup/go.mod)

- Adicionar `golang.org/x/time` (pacote `rate`)

---

## Verification Plan

### Automated Tests

#### Testes unitários do ThrottledWriter

Criar `throttle_test.go` com:

1. **TestThrottledWriter_RespectsBandwidthLimit** — Escreve N bytes com limit de X bytes/s, verifica que demorou pelo menos `N/X` segundos (com margem de 20%)
2. **TestThrottledWriter_ZeroBypasses** — Verifica que `NewThrottledWriter(w, 0)` retorna o writer original
3. **TestThrottledWriter_SmallWrites** — Múltiplas escritas pequenas (<burst) funcionam corretamente

```bash
cd /home/lucas/Projects/n-backup && go test ./internal/agent/ -run TestThrottled -v
```

#### Testes de config

Adicionar em `config_test.go`:

4. **TestLoadAgentConfig_BandwidthLimit** — Config com `bandwidth_limit: 10mb` parseia para `10*1024*1024`
5. **TestLoadAgentConfig_BandwidthLimitTooLow** — Config com `bandwidth_limit: 32kb` retorna erro (mínimo 64kb)
6. **TestLoadAgentConfig_BandwidthLimitEmpty** — Config sem `bandwidth_limit` → `BandwidthLimitRaw == 0` (sem throttle)

```bash
cd /home/lucas/Projects/n-backup && go test ./internal/config/ -run TestLoadAgentConfig_Bandwidth -v
```

#### Testes existentes (regressão)

```bash
cd /home/lucas/Projects/n-backup && go test ./...
```

### Manual Verification

Testar com deploy real é responsabilidade do usuário — a verificação automatizada valida a lógica do throttle e parsing de config.
