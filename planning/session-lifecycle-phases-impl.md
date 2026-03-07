# Session Lifecycle Phases — Plano de Implementação

Adicionar visibilidade das fases pós-streaming (Verifying, Uploading) ao ciclo de vida da sessão, tanto no WebUI quanto nos logs. Hoje, quando o streaming termina, a sessão desaparece imediatamente, deixando o operador sem feedback durante verificação de integridade (~minutos para 424GB) e uploads para Object Storage.

## User Review Required

> [!IMPORTANT]
> A sessão permanece registrada no `sync.Map` de sessions durante Verifying e Uploading. Isso significa que a sessão continua visível no WebUI e na API até o pipeline pós-commit completar. Atualmente ela é deletada logo após o commit.

> [!IMPORTANT]
> A `VerifyArchiveIntegrity` muda de `func(path string) error` para `func(path string, progress *IntegrityProgress) error`. O progress é **opcional** (nil = sem tracking). Isso é uma mudança de assinatura pública.

---

## Proposta de Fases

As fases da sessão são:

```
receiving → assembling → verifying → uploading → done / failed
```

Fases opcionais:
- `verifying` — só quando `verify_integrity: true`
- `uploading` — só quando há `buckets` configurados

---

## Proposed Changes

### Fase 1: DTOs e Progress Structs

#### [MODIFY] [dto.go](file:///home/lucas/Projects/n-backup/internal/server/observability/dto.go)

- Adicionar campo `Phase string` ao `SessionSummary` (json `"phase"`)
- Adicionar `IntegrityProgressDTO` e `PostCommitProgressDTO`
- Incluir ambos como campos opcionais no `SessionDetail`

```go
// No SessionSummary
Phase string `json:"phase"` // receiving | assembling | verifying | uploading | done | failed

// Novos DTOs
type IntegrityProgressDTO struct {
    BytesRead   int64   `json:"bytes_read"`
    TotalBytes  int64   `json:"total_bytes"`
    Entries     int64   `json:"entries"`
    ProgressPct float64 `json:"progress_pct"`
    ETA         string  `json:"eta,omitempty"`
}

type PostCommitProgressDTO struct {
    Bucket       string  `json:"bucket"`
    Mode         string  `json:"mode"`
    BytesSent    int64   `json:"bytes_sent"`
    TotalBytes   int64   `json:"total_bytes"`
    ProgressPct  float64 `json:"progress_pct"`
}

// No SessionDetail
IntegrityProgress  *IntegrityProgressDTO  `json:"integrity_progress,omitempty"`
PostCommitProgress *PostCommitProgressDTO  `json:"post_commit_progress,omitempty"`
```

---

#### [NEW] [session_phase.go](file:///home/lucas/Projects/n-backup/internal/server/session_phase.go)

Structs atômicos para progresso lock-free (mesmo padrão do `SyncProgress`):

```go
type SessionPhase struct {
    Phase atomic.Value // string: receiving | assembling | verifying | uploading | done | failed
}

type IntegrityProgress struct {
    BytesRead  atomic.Int64
    TotalBytes atomic.Int64
    Entries    atomic.Int64
    StartedAt  atomic.Value // time.Time
}

type PostCommitProgress struct {
    Bucket     atomic.Value // string
    Mode       atomic.Value // string
    BytesSent  atomic.Int64
    TotalBytes atomic.Int64
}
```

---

### Fase 2: Refatorar `integrity.go`

#### [MODIFY] [integrity.go](file:///home/lucas/Projects/n-backup/internal/server/integrity.go)

- Mudar assinatura: `VerifyArchiveIntegrity(path string, progress *IntegrityProgress) error`
- Quando `progress != nil`:
  - Setar `TotalBytes` com `fi.Size()` no início
  - Usar `io.TeeReader` com um counting wrapper para atualizar `BytesRead` atomicamente
  - Incrementar `Entries` a cada entry do tar
- Log periódico a cada ~30s com bytes lidos, entries e taxa

---

### Fase 3: Refatorar `post_commit.go`

#### [MODIFY] [post_commit.go](file:///home/lucas/Projects/n-backup/internal/server/post_commit.go)

- Adicionar `Progress *PostCommitProgress` ao `PostCommitOrchestrator`
- No `Execute()`, antes de cada goroutine iniciar o upload:
  - Atualizar `Progress.Bucket` e `Progress.Mode`
  - Atualizar `Progress.TotalBytes` com o tamanho do arquivo
- O progresso de bytes enviados (`BytesSent`) será atualizado via o reader do upload (será necessário wrapping mínimo)

> [!NOTE]
> Como os uploads são paralelos por bucket, o progress mostra o bucket atualmente mais ativo. Para v1 isso é suficiente. Múltiplos progresses por bucket seria over-engineering.

---

### Fase 4: Transições de Fase nos Handlers

#### [MODIFY] [handler_parallel.go](file:///home/lucas/Projects/n-backup/internal/server/handler_parallel.go)

- Adicionar `Phase *SessionPhase`, `IntProgress *IntegrityProgress`, `PCProgress *PostCommitProgress` ao `ParallelSession`
- Inicializar `Phase` com `"receiving"` na criação
- Transições em `handleParallelBackup`:
  - Após `Closing.Store(true)` → `phase = "assembling"`
  - Em `validateAndCommitWithTrailer`:
    - Antes de `VerifyArchiveIntegrity` → `phase = "verifying"`, inicializar `IntProgress`
    - Antes de `runPostCommitSync` → `phase = "uploading"`, inicializar `PCProgress`
    - Antes de `recordSessionEnd` → `phase = "done"` ou `"failed"`
- **Mover `h.sessions.Delete(sessionID)` para DEPOIS de phase="done"**, mantendo sessão visível

#### [MODIFY] [handler_single.go](file:///home/lucas/Projects/n-backup/internal/server/handler_single.go)

- Adicionar `Phase *SessionPhase`, `IntProgress *IntegrityProgress`, `PCProgress *PostCommitProgress` ao `PartialSession`
- Inicializar com `"receiving"` na criação
- Transições em `validateAndCommit`:
  - Antes de `VerifyArchiveIntegrity` → `phase = "verifying"`
  - Antes de `runPostCommitSync` → `phase = "uploading"`
  - Final → `phase = "done"` ou `"failed"`
- **Mover `h.sessions.Delete(sessionID)` para DEPOIS de phase="done"**, mantendo sessão visível

---

### Fase 5: Observabilidade (Snapshot)

#### [MODIFY] [handler_observability.go](file:///home/lucas/Projects/n-backup/internal/server/handler_observability.go)

- Em `SessionsSnapshot()`:
  - Preencher `Phase` no `SessionSummary` a partir de `SessionPhase.Phase`
  - Default: derivar do status existente quando Phase não está setado (backward compat)
- Em `SessionDetail()`:
  - Cuando `phase == "verifying"` → popular `IntegrityProgressDTO` a partir do `IntegrityProgress`
  - Quando `phase == "uploading"` → popular `PostCommitProgressDTO` a partir do `PostCommitProgress`

---

### Fase 6: Frontend WebUI

#### [MODIFY] Arquivos JS/HTML do WebUI em `internal/server/observability/web/`

- **Session List**: Badge colorido de fase:
  - `receiving` = azul, `assembling` = azul, `verifying` = amarelo, `uploading` = laranja, `done` = verde, `failed` = vermelho
- **Session Detail**: Indicador visual:
  - Verifying: barra de progresso (bytes lidos / total) + ETA
  - Uploading: progresso por bucket (bytes enviados / total) + modo

---

### Fase 7: Logging Periódico

#### [MODIFY] [integrity.go](file:///home/lucas/Projects/n-backup/internal/server/integrity.go)

- Log estruturado a cada ~30s durante verificação:
  ```
  "msg":"integrity_progress", "bytes_read":"12.3 GB", "total":"424.0 GB", "entries":1523, "pct":"2.9%"
  ```
- Log final com duração, total de entries e bytes:
  ```
  "msg":"integrity_complete", "entries":45821, "bytes":"424.0 GB", "duration":"4m32s"
  ```

#### [MODIFY] [post_commit.go](file:///home/lucas/Projects/n-backup/internal/server/post_commit.go)

- Adicionar progresso percentual aos logs existentes de upload

---

## Verification Plan

### Testes Unitários

Todos executados com:
```bash
cd /home/lucas/Projects/n-backup && go test ./internal/server/... -v -run <TestName>
```

| Teste | Arquivo | O que verifica |
|-------|---------|---------------|
| `TestVerifyArchiveIntegrity_Progress` | `integrity_test.go` | `IntegrityProgress` é atualizado durante verificação (BytesRead > 0, Entries > 0 no final) |
| `TestVerifyArchiveIntegrity_NilProgress` | `integrity_test.go` | Backward compat: progress=nil não causa panic |
| `TestSessionPhase_Transitions` | `session_phase_test.go` | Transições atômicas de fase funcionam corretamente |
| `TestPostCommit_Progress` | `post_commit_test.go` | `PostCommitProgress` contém bucket e mode corretos após Execute |
| Testes existentes | `integrity_test.go` | Todos os 7 testes existentes continuam passando com nova assinatura |
| Testes existentes | `post_commit_test.go` | Todos os 6 testes existentes continuam passando |

### Build Completo

```bash
cd /home/lucas/Projects/n-backup && go build ./...
```

### Testes Existentes (Regressão)

```bash
cd /home/lucas/Projects/n-backup && go test ./... -count=1
```

### Verificação Manual — WebUI

1. Executar backup com `verify_integrity: true` em storage com buckets configurados
2. Durante o backup, acessar WebUI e verificar que a sessão mostra badges de fase
3. Após streaming, verificar transição: `receiving → assembling → verifying → uploading → done`
4. Na sessão detail, verificar barra de progresso para Verifying e Uploading
