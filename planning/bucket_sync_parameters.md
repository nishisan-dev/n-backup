# Parâmetros `sync_strategy` e `async_upload` para Bucket Sync

Adicionar dois parâmetros opcionais ao `BucketConfig` que permitem ajustar o comportamento do sync pós-commit:
- **`sync_strategy`**: controla a ordem de upload vs delete no modo sync
- **`async_upload`**: controla se o sync/archive roda em background, liberando o lock e enviando o FinalACK antes

Ambos mantêm o comportamento atual como default — são opt-in para cenários específicos.

## Proposed Changes

### Config

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

1. Adicionar constantes de `SyncStrategy`:
```go
const (
    SyncStrategySafe            = "safe"             // default: upload primeiro, delete depois
    SyncStrategySpaceEfficient  = "space_efficient"  // delete primeiro, upload depois
)
```

2. Adicionar campos ao `BucketConfig`:
```go
SyncStrategy string `yaml:"sync_strategy"` // safe (default) | space_efficient
AsyncUpload  bool   `yaml:"async_upload"`  // false (default) | true
```

3. Atualizar `validateBuckets`:
   - `sync_strategy`: se vazio → `"safe"`. Aceita `safe` ou `space_efficient`. Só válido para `mode: sync` — rejeitar para outros modos.
   - `async_upload`: se `true` em `mode: offload` → erro (offload é sempre bloqueante).

---

### Post-Commit Orchestration

#### [MODIFY] [post_commit.go](file:///home/lucas/Projects/n-backup/internal/server/post_commit.go)

Alterar `executeSync` para respeitar `SyncStrategy`:

```go
func (o *PostCommitOrchestrator) executeSync(...) error {
    remotePath := bt.cfg.Prefix + filepath.Base(finalPath)

    if bt.cfg.SyncStrategy == config.SyncStrategySpaceEfficient {
        // Delete primeiro para liberar espaço no bucket
        for _, name := range rotatedFiles {
            remoteKey := bt.cfg.Prefix + name
            if err := bt.backend.Delete(ctx, remoteKey); err != nil {
                logger.Warn("sync space_efficient: delete failed (non-fatal)", "key", remoteKey, "error", err)
            }
        }
        // Depois faz upload do novo backup
        if err := o.uploadWithRetry(ctx, bt.backend, finalPath, remotePath, logger); err != nil {
            return fmt.Errorf("sync upload: %w", err)
        }
        return nil
    }

    // Default (safe): upload primeiro, delete depois — sem mudança
    // ... código existente ...
}
```

---

### Handlers

#### [MODIFY] [handler_single.go](file:///home/lucas/Projects/n-backup/internal/server/handler_single.go)

Em `validateAndCommitSingle`, após o Rotate e antes do `runPostCommitSync`, verificar se todos os buckets ativos (excluindo archive) têm `async_upload: true`. Se sim:

1. Enviar `FinalACK(OK)` **antes** do sync
2. Liberar o lock (defer é rearranjado)
3. Disparar `runPostCommitSync` em goroutine background

Para isso, a lógica de FinalACK e lock precisa ser extraída. A abordagem mais limpa é:

```go
// Após Rotate...
if shouldAsyncUpload(storageInfo.Buckets) {
    // Envia ACK antes do sync — agent pode iniciar novo backup
    protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
    // Libera lock para permitir novas sessões
    h.locks.Delete(lockKey)
    // Sync em background (fire-and-forget com logging)
    go h.runPostCommitSync(storageInfo, finalPath, removed, writer.AgentDir(), logger)
    return "ok", dataSize
}

// Comportamento padrão (síncrono)
h.runPostCommitSync(storageInfo, finalPath, removed, writer.AgentDir(), logger)
protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
return "ok", dataSize
```

> [!IMPORTANT]
> Como o `handleBackup` usa `defer h.locks.Delete(lockKey)`, quando `async_upload: true`, precisamos deletar o lock explicitamente **antes** do defer rodar. O `defer` será idempotente (delete de chave inexistente é noop no `sync.Map`).

#### [MODIFY] [handler_parallel.go](file:///home/lucas/Projects/n-backup/internal/server/handler_parallel.go)

Mesma lógica em `validateAndCommitWithTrailer`. No handler paralelo, o lock é liberado pelo `defer h.locks.Delete(lockKey)` no início de `handleParallelBackup`. A abordagem é a mesma: liberar explicitamente e disparar goroutine.

---

### Helpers

#### [MODIFY] [post_commit_helpers.go](file:///home/lucas/Projects/n-backup/internal/server/post_commit_helpers.go)

Adicionar helper `shouldAsyncUpload`:

```go
// shouldAsyncUpload retorna true se TODOS os buckets não-archive têm async_upload habilitado.
// Se não há buckets ativos (excluindo archive), retorna false.
func shouldAsyncUpload(buckets []config.BucketConfig) bool {
    active := filterBucketsExcluding(buckets, config.BucketModeArchive)
    if len(active) == 0 {
        return false
    }
    for _, b := range active {
        if !b.AsyncUpload {
            return false
        }
    }
    return true
}
```

---

### Documentação

#### [MODIFY] [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md)

Atualizar seção **4.3 Object Storage Pós-Commit** (linha ~710):
- Adicionar `sync_strategy` e `async_upload` ao exemplo YAML de buckets
- Documentar os valores válidos e restrições por modo na tabela de modos

#### [MODIFY] [usage.md](file:///home/lucas/Projects/n-backup/docs/usage.md)

Adicionar nova seção **Object Storage Pós-Commit** após a seção de Rotação Automática (linha ~529), documentando:
- Configuração de buckets com exemplos YAML incluindo os novos parâmetros
- Tabela de modos com `sync_strategy` e `async_upload`
- Notas sobre tradeoffs de cada opção

#### [MODIFY] [post_commit_sync.puml](file:///home/lucas/Projects/n-backup/docs/diagrams/post_commit_sync.puml)

Atualizar o diagrama de sequência para refletir:
- Bifurcação no sync mode entre `safe` e `space_efficient`
- Nota sobre `async_upload` liberando FinalACK antes do sync

---

## Verification Plan

### Automated Tests

Os testes existentes cobrem os fluxos `sync`, `offload`, `archive` e combinações. As mudanças precisam:

1. **Testes de config** (`internal/config/config_test.go`):
   - `sync_strategy` default para `"safe"` quando ausente
   - `sync_strategy: space_efficient` aceito para `mode: sync`
   - `sync_strategy` rejeitado para `mode: offload` e `mode: archive`
   - `async_upload: true` rejeitado para `mode: offload`
   - `async_upload: true` aceito para `mode: sync`

2. **Testes de sync strategy** (`internal/server/post_commit_test.go`):
   - Novo test: `TestPostCommit_SyncMode_SpaceEfficient` — verifica que delete é chamado **antes** do upload
   - O teste existente `TestPostCommit_SyncMode` continua validando o comportamento `safe` (inalterado)

3. **Teste de async upload** (`internal/server/post_commit_test.go`):
   - Novo test: `TestShouldAsyncUpload` — valida a lógica do helper

4. **Regressão**: rodar todos os testes existentes

**Comando para rodar os testes:**
```bash
cd /home/lucas/Projects/n-backup && go test ./internal/config/ ./internal/server/ -v -count=1 -run "TestPostCommit|TestLoadServerConfig|TestShouldAsync"
```

### Documentação
- Verificar que `specification.md` reflete os novos parâmetros no exemplo YAML e tabela de modos
- Verificar que `usage.md` contém a nova seção de Object Storage com exemplos completos
- Verificar que o diagrama `post_commit_sync.puml` renderiza sem erros de sintaxe
