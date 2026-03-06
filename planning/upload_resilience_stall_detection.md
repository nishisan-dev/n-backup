# Upload Resilience — Stall Detection e Limpeza de Multipart

Upload de arquivos grandes (>100GB) falha com timeout fixo de 30min (`context deadline exceeded`). Cada retry recomeça do zero, desperdiçando horas. Multipart uploads incompletos geram lixo no bucket.

## Diagnóstico

O timeout atual é um **deadline global** — não importa se dados ainda estão fluindo. Um arquivo de 456GB numa conexão de 5MB/s precisaria de ~25h, mas o timeout mata após 30min.

## Abordagem: Stall Detection

Em vez de um deadline total, usamos **detecção de inatividade** (stall):

- Wrapeamos o `io.Reader` do arquivo em um `stallDetectReader` que reseta um timer a cada `Read()`.
- Se nenhum byte for lido em **5 minutos** (default `stall_timeout`), o contexto é cancelado.
- **Enquanto bytes fluem, o upload nunca é abortado** — não importa se demora 1h ou 24h.
- O operador pode configurar `stall_timeout` no YAML para ajustar a tolerância.

> [!IMPORTANT]
> Isso elimina completamente o antipadrão de "chutar" um timeout global baseado em tamanho.
> O sistema se adapta naturalmente a qualquer banda e qualquer tamanho de arquivo.

---

## Proposed Changes

### Componente: `internal/objstore`

#### [MODIFY] [backend.go](file:///home/lucas/Projects/n-backup/internal/objstore/backend.go)

- Adicionar `AbortIncompleteUploads(ctx, prefix) error` à interface `Backend`.

#### [MODIFY] [s3.go](file:///home/lucas/Projects/n-backup/internal/objstore/s3.go)

- Implementar `AbortIncompleteUploads` via `ListMultipartUploads` + `AbortMultipartUpload`.
- Refatorar `Upload` para aceitar um `io.Reader` wrapeado ao invés de abrir o arquivo internamente.
  - Na verdade, manter o `Upload(ctx, localPath, remotePath)` mas internamente usar o stall reader.
  - Adicionar os campos `stallTimeout` ao `S3Backend` (injetado na criação).
  - Criar contexto interno com cancel, que o stall reader cancela ao detectar inatividade.

#### [MODIFY] [mock.go](file:///home/lucas/Projects/n-backup/internal/objstore/mock.go)

- Implementar `AbortIncompleteUploads` no mock (no-op com registro de chamadas).

#### [NEW] [stall_reader.go](file:///home/lucas/Projects/n-backup/internal/objstore/stall_reader.go)

- `stallDetectReader` — wraps `io.Reader`, reseta timer em cada `Read()`.
- `Read()`: lê do reader interno, se sucesso (n > 0) reseta o timer.
- Timer goroutine: ao expirar, chama `cancelFunc()` do contexto.
- `Close()`: para o timer (cleanup).
- Constante default: `defaultStallTimeout = 5 * time.Minute`.

#### [NEW] [stall_reader_test.go](file:///home/lucas/Projects/n-backup/internal/objstore/stall_reader_test.go)

- Testes: reader normal não stalla, reader com delay > threshold aciona cancel.

---

### Componente: `internal/config`

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

- Adicionar `StallTimeout time.Duration` (YAML `stall_timeout`) ao `BucketConfig`.
- Default: `0` → usa `5 * time.Minute`.
- Validação: se `> 0`, aceita como override.

---

### Componente: `internal/server`

#### [MODIFY] [post_commit.go](file:///home/lucas/Projects/n-backup/internal/server/post_commit.go)

- `uploadWithRetry`: remover o `context.WithTimeout` de 30min.
  - O stall detection interno ao `S3Backend.Upload` substitui o papel do timeout.
- Após falha final, chamar `backend.AbortIncompleteUploads(ctx, remotePath)`.
- Passar `stallTimeout` ao backend via `BucketConfig`.

#### [MODIFY] [sync_storage.go](file:///home/lucas/Projects/n-backup/internal/server/sync_storage.go)

- `uploadWithRetryStandalone`: mesma remoção de `context.WithTimeout`.
- Após falha final, chamar `backend.AbortIncompleteUploads`.

#### [MODIFY] [post_commit_helpers.go](file:///home/lucas/Projects/n-backup/internal/server/post_commit_helpers.go)

- `defaultBackendFactory`: propagar `StallTimeout` do `BucketConfig` ao `S3Config`.

---

## Verification Plan

### Automated Tests

```bash
# Todos os testes com race detector
go test -v -race ./internal/objstore/...
go test -v -race ./internal/server/...
go test -v -race ./internal/config/...

# Build completo
go build ./...
```

### Testes novos esperados

- `TestStallDetectReader_NormalRead`: reader funcional, sem stall.
- `TestStallDetectReader_StallCancelsContext`: reader que para de enviar → contexto cancelado.
- `TestS3Backend_AbortIncompleteUploads`: mock de ListMultipartUploads + abort.
- Testes existentes continuam passando (sem regressão).
