# Sync Jobs — Observabilidade no WebUI

Expor os sync jobs do Object Storage no WebUI com métricas de observabilidade em tempo real: status (idle/running/completed), progresso (arquivos uploaded/skipped/errors), ETA, MB/s, tamanho dos arquivos, e duração por bucket.

## Contexto Atual

O Handler já possui:
- `syncRunning atomic.Bool` — estado atômico do sync
- `lastSyncResult atomic.Value` — resultado da última sincronização (`*SyncStorageResult`)
- Structs `SyncStorageResult` e `SyncBucketResult` com tags JSON

O que **não existe** hoje:
- Endpoint API para consultar sync status
- Método na interface `HandlerMetrics` para sync
- Progresso em tempo real (contadores atômicos de arquivos processados durante o sync)
- Representação no WebUI (card, tabela, badges)

## User Review Required

> [!IMPORTANT]
> **Decisão sobre granularidade do progresso em tempo real**: O sync atual só armazena resultado final. Para mostrar ETA e MB/s durante o sync, precisamos adicionar contadores atômicos ao processo. Isso envolve uma `SyncProgress` struct com campos atômicos que é atualizada a cada arquivo processado no `syncOneBucket`. O custo é mínimo (poucos campos atômicos, sem contention).

> [!NOTE]
> **Sem breaking changes**: Apenas adição de novo endpoint, novos DTOs e novos componentes de UI. Nenhuma alteração de comportamento existente.

---

## Proposed Changes

### Componente: Backend — Progresso em Tempo Real

#### [MODIFY] [sync_storage.go](file:///home/lucas/Projects/n-backup/internal/server/sync_storage.go)

Adicionar struct `SyncProgress` com contadores atômicos para progresso em tempo real:

```go
// SyncProgress rastreia o progresso em tempo real de uma sincronização ativa.
// Campos atômicos para leitura lock-free pelo endpoint HTTP.
type SyncProgress struct {
    Running      atomic.Bool
    StartedAt    atomic.Value  // time.Time
    CurrentFile  atomic.Value  // string (path relativo do arquivo sendo enviado)
    CurrentBucket atomic.Value // string (nome do bucket atual)
    TotalFiles   atomic.Int64  // total de arquivos a processar (todos os buckets)
    ProcessedFiles atomic.Int64 // arquivos já processados (uploaded + skipped)
    UploadedFiles atomic.Int64
    SkippedFiles  atomic.Int64
    ErrorFiles    atomic.Int64
    BytesUploaded atomic.Int64  // bytes total enviados (soma de file sizes uploaded)
}
```

Modificar `SyncExistingStorage` e `syncOneBucket` para atualizar `SyncProgress` a cada arquivo:
- Antes de iniciar: set `Running=true`, `StartedAt`, zerar contadores
- Em cada arquivo: atualizar `ProcessedFiles`, `UploadedFiles`/`SkippedFiles`/`ErrorFiles`, `CurrentFile`, `BytesUploaded`
- Ao finalizar: set `Running=false`

---

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

Adicionar campo `syncProgress *SyncProgress` ao `Handler`:
```go
// syncProgress rastreia progresso em tempo real do sync retroativo.
syncProgress SyncProgress
```

---

### Componente: Backend — Interface e DTOs

#### [MODIFY] [dto.go](file:///home/lucas/Projects/n-backup/internal/server/observability/dto.go)

Adicionar DTOs para o status do sync:

```go
// SyncStatusDTO é retornado por GET /api/v1/sync/status.
type SyncStatusDTO struct {
    Running       bool              `json:"running"`
    StartedAt     string            `json:"started_at,omitempty"`
    Elapsed       string            `json:"elapsed,omitempty"`
    Progress      *SyncProgressDTO  `json:"progress,omitempty"`   // nil quando idle
    LastResult    *SyncResultDTO    `json:"last_result,omitempty"` // nil se nunca rodou
}

// SyncProgressDTO contém métricas de progresso em tempo real.
type SyncProgressDTO struct {
    CurrentFile    string  `json:"current_file"`
    CurrentBucket  string  `json:"current_bucket"`
    TotalFiles     int64   `json:"total_files"`
    ProcessedFiles int64   `json:"processed_files"`
    UploadedFiles  int64   `json:"uploaded_files"`
    SkippedFiles   int64   `json:"skipped_files"`
    ErrorFiles     int64   `json:"error_files"`
    BytesUploaded  int64   `json:"bytes_uploaded"`
    ProgressPct    float64 `json:"progress_pct"` // 0-100
    ETA            string  `json:"eta,omitempty"`
}

// SyncResultDTO resume o último resultado completo.
type SyncResultDTO struct {
    StartedAt string `json:"started_at"`
    EndedAt   string `json:"ended_at"`
    Duration  string `json:"duration"`
    Uploaded  int    `json:"uploaded"`
    Skipped   int    `json:"skipped"`
    Errors    int    `json:"errors"`
    Buckets   []SyncBucketDTO `json:"buckets"`
}

// SyncBucketDTO resume resultado por bucket.
type SyncBucketDTO struct {
    StorageName string `json:"storage_name"`
    BucketName  string `json:"bucket_name"`
    Mode        string `json:"mode"`
    Uploaded    int    `json:"uploaded"`
    Skipped     int    `json:"skipped"`
    Errors      int    `json:"errors"`
    Duration    string `json:"duration"`
    Error       string `json:"error,omitempty"`
}
```

#### [MODIFY] [http.go](file:///home/lucas/Projects/n-backup/internal/server/observability/http.go)

1. Adicionar `SyncStatusSnapshot() SyncStatusDTO` à interface `HandlerMetrics`
2. Registrar endpoint: `mux.HandleFunc("GET /api/v1/sync/status", makeSyncStatusHandler(metrics))`
3. Implementar `makeSyncStatusHandler`

#### [MODIFY] [handler_observability.go](file:///home/lucas/Projects/n-backup/internal/server/handler_observability.go)

Implementar `SyncStatusSnapshot() observability.SyncStatusDTO` no Handler, lendo de `syncProgress` (contadores atômicos) e `lastSyncResult` (último resultado).

---

### Componente: Backend — Métricas Prometheus

#### [MODIFY] [http.go](file:///home/lucas/Projects/n-backup/internal/server/observability/http.go)

Adicionar ao `makePrometheusHandler`:
```
nbackup_server_sync_running 0|1
nbackup_server_sync_files_uploaded_total N
nbackup_server_sync_files_skipped_total N
nbackup_server_sync_files_errors_total N
nbackup_server_sync_bytes_uploaded_total N
```

---

### Componente: Frontend — SPA

#### [MODIFY] [api.js](file:///home/lucas/Projects/n-backup/internal/server/observability/web/js/api.js)

Adicionar:
```js
syncStatus(init) { return this.get('/api/v1/sync/status', init); },
```

#### [MODIFY] [components.js](file:///home/lucas/Projects/n-backup/internal/server/observability/web/js/components.js)

Adicionar `renderSyncStatusCard(sync)` — card com:
- **Header**: "Object Storage Sync" + badge (Idle/Running/Completed)
- **Quando running**: barra de progresso, arquivo atual, bucket, files uploaded/skipped/errors, bytes uploaded, ETA
- **Quando idle com last_result**: resumo do último sync (duração, uploaded, skipped, errors) + tabela de buckets
- **Quando idle sem last_result**: mensagem "Nenhum sync executado"

#### [MODIFY] [app.js](file:///home/lucas/Projects/n-backup/internal/server/observability/web/js/app.js)

Em `fetchOverview()`, adicionar chamada a `API.syncStatus()` no `Promise.all` e chamar `Components.renderSyncStatusCard()`.

#### [MODIFY] [index.html](file:///home/lucas/Projects/n-backup/internal/server/observability/web/index.html)

Adicionar container para o card de sync no overview, após o chunk-buffer-card:
```html
<div class="card sync-status-card" id="sync-status-card" style="display:none"></div>
```

#### [MODIFY] [style.css](file:///home/lucas/Projects/n-backup/internal/server/observability/web/css/style.css)

Adicionar estilos para:
- `.sync-status-card` (card base)
- `.sync-badge` (idle/running/completed)
- `.sync-progress-bar` (barra de progresso com animação)
- `.sync-file-current` (truncated ellipsis para path longo)
- `.sync-result-table` (tabela de buckets no resultado)

---

### Componente: Testes

#### [MODIFY] [http_test.go](file:///home/lucas/Projects/n-backup/internal/server/observability/http_test.go)

Adicionar métodos ao `mockMetrics`:
- `SyncStatusSnapshot() SyncStatusDTO` — retorna dados mockados

Adicionar testes:
- `TestSyncStatus_Idle` — sem sync rodando, sem resultado anterior
- `TestSyncStatus_Running` — com progresso ativo
- `TestSyncStatus_WithLastResult` — idle com resultado do último sync

---

### Componente: Documentação

#### [MODIFY] [WebUI.md](file:///home/lucas/Projects/n-backup/wiki/WebUI.md)

Adicionar na seção "Funcionalidades" > "Overview":
- "**Object Storage Sync**: Status e progresso de sincronizações retroativas"

Adicionar na tabela de API REST:
- `| /api/v1/sync/status | GET | Status e progresso do sync retroativo |`

---

## Verification Plan

### Automated Tests

```bash
# 1. Testes da camada HTTP/API (inclui novos testes de sync status)
cd /home/lucas/Projects/n-backup && go test ./internal/server/observability/... -v -run "Sync"

# 2. Testes do sync_storage (existentes + novos de progresso atômico)
cd /home/lucas/Projects/n-backup && go test ./internal/server/... -v -run "Sync"

# 3. Build completo (verifica que nada quebrou)
cd /home/lucas/Projects/n-backup && go build ./...

# 4. Race detector nos testes de sync (validar contadores atômicos)
cd /home/lucas/Projects/n-backup && go test ./internal/server/... -race -run "Sync"

# 5. Suite completa
cd /home/lucas/Projects/n-backup && go test ./... -race
```

### Manual Verification

O usuário pode verificar o WebUI manualmente:

1. Iniciar o server com `web_ui.enabled: true`
2. Acessar `http://127.0.0.1:9848` → Overview
3. Verificar que o card "Object Storage Sync" aparece (status Idle se não houver sync recente)
4. Disparar sync com `nbackup-server sync-storage` (SIGUSR1)
5. Observar o card atualizar em tempo real: barra de progresso, arquivo atual, ETA, MB/s
6. Após concluir, verificar que o card mostra o resumo do último sync com tabela de buckets
