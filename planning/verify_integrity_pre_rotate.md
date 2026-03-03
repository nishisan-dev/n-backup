# Validação de Integridade Pré-Rotate

Após o `Commit()` (rename atômico do `.tmp` para nome final com timestamp), o server executa o `Rotate()` que apaga backups excedentes. Hoje não há nenhuma validação de que o backup recém-gerado é legível/íntegro antes de apagar os antigos. O usuário faz isso manualmente com:

```bash
pv arquivo.tar.zst | tar -I zstd -tf - > /dev/null
```

## Objetivo

Automatizar essa validação: após o `Commit()` e **antes** do `Rotate()`, o server verifica a integridade do arquivo comprimido recém-commitado lendo e descomprimindo o tarball inteiro. Se a validação falhar, o rotate **não acontece** — o backup novo fica no disco mas nenhum antigo é apagado (fail-safe). Um evento de erro é emitido para a WebUI.

## Proposta de Configuração

Opção `verify_integrity` no nível do storage, com 3 valores possíveis:

| Valor | Comportamento |
|:---:|:---|
| `false` (default) | Sem verificação — comportamento atual |
| `true` | Verifica integridade; se falhar, **pula** o rotate (fail-safe) |

Ficaria assim no YAML:

```yaml
storages:
  scripts:
    base_dir: /var/backups/scripts
    max_backups: 5
    compression_mode: zst
    verify_integrity: true    # <-- NOVO
```

> [!IMPORTANT]
> A verificação lê o arquivo inteiro do disco novamente, o que adiciona I/O proporcional ao tamanho do backup. Para backups muito grandes (dezenas de GB), isso pode aumentar significativamente o tempo entre o commit e o FinalACK.

---

## Proposed Changes

### Config

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

- Adicionar campo `VerifyIntegrity bool` em `StorageInfo` com tag YAML `verify_integrity`
- Sem validação especial necessária — `false` é o zero-value e o default desejado

---

### Core — Verificação de Integridade

#### [NEW] [integrity.go](file:///home/lucas/Projects/n-backup/internal/server/integrity.go)

Nova função `VerifyArchiveIntegrity(path string) error`:

1. Detecta o tipo de compressão pela extensão (`.tar.gz` → gzip, `.tar.zst` → zstd)
2. Abre o arquivo, cria um reader de descompressão, e wrapa num `tar.Reader`
3. Itera todos os entries com `tar.Next()` e drena seus conteúdos com `io.Copy(io.Discard, tr)`
4. Retorna `nil` se chegar ao `io.EOF` sem erros, ou o erro encontrado

Dependências:
- `compress/gzip` (stdlib)
- `github.com/klauspost/compress/zstd` — verificar se já está no `go.mod` do projeto. Se não, será adicionado.

> [!NOTE]
> Esta abordagem replica exatamente o que `tar -I zstd -tf -` faz: lê e descomprime cada bloco do archive, validando a estrutura tar e a integridade da compressão end-to-end.

---

### Handler — Integração no Fluxo

#### [MODIFY] [handler_single.go](file:///home/lucas/Projects/n-backup/internal/server/handler_single.go)

Em `validateAndCommit()`, ENTRE o `writer.Commit()` (L444) e o bloco de archive pre-rotate (L451):

```go
// Verifica integridade do backup recém-commitado antes de rotacionar
if storageInfo.VerifyIntegrity {
    logger.Info("verifying backup integrity", "path", finalPath)
    if err := VerifyArchiveIntegrity(finalPath); err != nil {
        logger.Error("backup integrity check failed — skipping rotation",
            "path", finalPath, "error", err)
        if h.Events != nil {
            h.Events.PushEvent("error", "integrity_failed", writer.AgentName(),
                fmt.Sprintf("integrity check failed for %s: %v", finalPath, err), 0)
        }
        // Backup fica no disco mas NÃO apaga antigos (fail-safe)
        protocol.WriteFinalACK(conn, protocol.FinalStatusOK)
        return "ok", dataSize
    }
    logger.Info("backup integrity verified", "path", finalPath)
}
```

#### [MODIFY] [handler_parallel.go](file:///home/lucas/Projects/n-backup/internal/server/handler_parallel.go)

Em `validateAndCommitWithTrailer()`, mesmo padrão: inserir ENTRE `writer.Commit()` (L698) e archive pre-rotate (L705).

---

### Configuração de Exemplo

#### [MODIFY] [server.example.yaml](file:///home/lucas/Projects/n-backup/configs/server.example.yaml)

Adicionar o campo `verify_integrity` com comentário explicativo nos dois storages de exemplo.

---

### Testes

#### [NEW] [integrity_test.go](file:///home/lucas/Projects/n-backup/internal/server/integrity_test.go)

Testes unitários para `VerifyArchiveIntegrity`:

1. **TestVerifyArchiveIntegrity_ValidTarGz** — cria um `.tar.gz` válido programaticamente, verifica que retorna `nil`
2. **TestVerifyArchiveIntegrity_ValidTarZst** — cria um `.tar.zst` válido, verifica `nil`
3. **TestVerifyArchiveIntegrity_CorruptTarGz** — grava bytes aleatórios com extensão `.tar.gz`, espera erro
4. **TestVerifyArchiveIntegrity_CorruptTarZst** — idem para `.tar.zst`
5. **TestVerifyArchiveIntegrity_TruncatedArchive** — trunca um archive válido pela metade, espera erro
6. **TestVerifyArchiveIntegrity_EmptyFile** — arquivo vazio, espera erro

---

## Verification Plan

### Testes Automatizados

```bash
cd /home/lucas/Projects/n-backup
go test -race -v ./internal/server/ -run TestVerifyArchiveIntegrity
go test -race -v ./internal/config/ -run TestStorageInfo
go test -race -v ./internal/server/ -run TestRotate
```

### Build Completa

```bash
cd /home/lucas/Projects/n-backup
go build ./...
```

### Verificação Manual

O usuário pode testar end-to-end executando um backup real com `verify_integrity: true` habilitado no YAML e observando os logs do server para as mensagens `verifying backup integrity` e `backup integrity verified`.
