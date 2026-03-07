# Feature: Session Lifecycle Phases no WebUI

## Contexto

Atualmente, quando um backup paralelo termina o streaming, a sessão fecha imediatamente no WebUI.
Porém, existem etapas significativas que ocorrem **após** o streaming mas **antes** de o backup estar realmente concluído:

1. **Verificação de integridade** (`verify_integrity: true`) — lê e descomprime o tar inteiro (~424 GB pode levar minutos)
2. **Upload para Object Storage** (PostCommit) — upload multipart para S3 de arquivos potencialmente grandes

O operador não tem visibilidade dessas etapas e pode assumir que o sistema travou.

### Motivação (Log real — 2026-03-06)

```
"msg":"parallel assembly complete, waiting for trailer" ... totalBytes:455312368727
"msg":"verifying backup integrity" ... path:.../2026-03-06T18-20-10-533.tar.zst
[... silêncio total ...]
```

Para um arquivo de ~424 GB com zstd, a verificação pode levar vários minutos sem nenhum feedback.

---

## Proposta

### Ciclo de vida da sessão (novo)

```
Receiving → Assembling → Verifying → Uploading → Done
                              │            │
                              │            └─ PostCommit (sync/offload/archive)
                              │               - bytes enviados / total
                              │               - bucket atual
                              │               - modo (sync/offload/archive)
                              │
                              └─ VerifyArchiveIntegrity
                                 - bytes lidos / total
                                 - ETA estimado
                                 - entries verificados
```

Fases opcionais:
- **Verifying** — só aparece quando `verify_integrity: true` no storage
- **Uploading** — só aparece quando há `buckets` configurados no storage

A sessão só transita para **Done** após **todo** o pipeline pós-commit terminar.

### Componentes impactados

#### 1. Backend — `internal/server/`

- **`handler_observability.go`** — Adicionar fase (`phase`) ao snapshot de sessão. Valores: `receiving`, `assembling`, `verifying`, `uploading`, `done`, `failed`

- **`integrity.go`** — Refatorar `VerifyArchiveIntegrity` para:
  - Aceitar um callback ou struct de progresso (`*IntegrityProgress`) como parâmetro
  - Reportar bytes lidos via `io.TeeReader` com `CountingReader`
  - Logar periodicamente (a cada ~10 GB ou ~30s)
  - Log final com duração, entries verificados, bytes lidos

- **`post_commit.go` / `post_commit_helpers.go`** — Refatorar para:
  - Aceitar um struct de progresso (`*PostCommitProgress`)
  - Reportar bytes enviados, bucket atual, modo
  - Atualizar progresso atomicamente para leitura pelo WebUI

- **`handler_single.go` / `handler_parallel.go`** — Atualizar `phase` da sessão em cada transição:
  - Após checksum OK → `phase = "verifying"` (se verify_integrity habilitado)
  - Após integrity OK → `phase = "uploading"` (se buckets configurados)
  - Após PostCommit completo → `phase = "done"`

#### 2. DTOs — `internal/server/observability/dto.go`

- Adicionar campo `Phase string` ao `SessionSummary` e `SessionDetail`
- Adicionar structs `IntegrityProgressDTO` e `PostCommitProgressDTO`
- Incluir no `SessionDetail` quando a fase correspondente estiver ativa

#### 3. Frontend — `internal/server/observability/web/`

- **Session Detail** — Mostrar indicador visual da fase atual:
  - Verifying: barra de progresso (bytes lidos / total) + ETA
  - Uploading: progresso por bucket (bytes enviados / total) + modo
- **Session List** — Badge de fase colorido (receiving=azul, verifying=amarelo, uploading=laranja, done=verde)

#### 4. Logs

- `integrity.go` — Log periódico de progresso + log final com duração
- `post_commit.go` — Já tem logs, mas adicionar progresso percentual

---

## Decisões de Design

| # | Decisão | Justificativa |
|---|---------|---------------|
| 1 | Progresso via structs atômicos (não channels) | Mesma abordagem do `SyncProgress` — leitura lock-free pelo HTTP handler |
| 2 | Fase como campo da sessão existente | Não cria entidade nova — evolui o modelo existente |
| 3 | Manter sessão aberta até Done | Importante para offload (remoção local) — operador **precisa** ver que upload completou |
| 4 | Logging periódico no integrity | Mesmo sem WebUI, o log estruturado deve mostrar progresso |

---

## Verificação

### Testes unitários

- **`integrity_test.go`** — Verificar que `IntegrityProgress` é atualizado durante a verificação (bytes lidos, entry count)
- **`post_commit_test.go`** — Verificar que `PostCommitProgress` é atualizado durante upload mock
- **`handler_observability.go`** — Verificar que `SessionDetail` inclui phase e progress quando ativos
- **`dto.go`** / **`http_test.go`** — Verificar serialização JSON dos novos campos

### Testes manuais / WebUI

- Executar backup com `verify_integrity: true` em storage com buckets configurados
- Verificar no WebUI que a sessão mostra as fases Verifying → Uploading → Done
- Verificar que o progresso atualiza em tempo real (polling de 2s)

### Verificação de logs

- Verificar que integrity emite logs periódicos de progresso
- Verificar log final com duração e contagem de entries
