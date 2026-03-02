# Object Storage Pós-Assembly — Plano de Implementação

## Contexto

Após o assembly do backup (Commit + Rotate), o servidor atualmente apenas grava no disco local. A proposta é adicionar suporte a **múltiplos destinos de Object Storage** com diferentes modos de operação, executados em paralelo após o commit local.

## Nomenclatura Proposta

Sugiro nomes mais expressivos que os originais:

| Proposta do User | Sugestão        | Comportamento                                                                                           |
|-------------------|-----------------|---------------------------------------------------------------------------------------------------------|
| `mirror`          | **`sync`**      | Copia o backup para o bucket, mantendo o local intacto. Espelha 1:1.                                   |
| `push`            | **`offload`**   | Envia para o bucket e **apaga o local** após confirmação de upload. Libera disco.                       |
| `trash`           | **`archive`**   | Envia para o bucket **apenas os backups que seriam deletados** pelo Rotate. Requer `retain` (quantos manter no bucket). |

> [!NOTE]
> Os nomes `sync`/`offload`/`archive` são auto-explicativos e evitam ambiguidade com conceitos de lixeira ou git push. Se preferir manter os originais, basta trocar.

---

## Modelo de Configuração YAML

```yaml
storages:
  scripts:
    base_dir: /var/backups/scripts
    max_backups: 5
    compression_mode: gzip

    # Nova seção: destinos de object storage pós-commit
    buckets:
      - name: s3-mirror
        provider: s3            # s3 | gcs | azure | minio (futuro)
        endpoint: ""            # vazio = AWS default; preenchido = MinIO/compatível
        region: us-east-1
        bucket: my-backup-bucket
        prefix: "scripts/"      # prefixo dentro do bucket (opcional)
        mode: sync              # sync | offload | archive
        # retain: N/A           # sync espelha o local — sem retain próprio
        credentials:
          access_key_env: AWS_ACCESS_KEY_ID       # nome da env var
          secret_key_env: AWS_SECRET_ACCESS_KEY   # nome da env var

      - name: offload-nas
        provider: s3
        endpoint: "https://minio.internal:9000"
        bucket: primary-backups
        prefix: "scripts/"
        mode: offload           # envia + apaga local
        retain: 10              # obrigatório: rotação no bucket (bucket é o storage primário)
        credentials:
          access_key_env: MINIO_ACCESS_KEY
          secret_key_env: MINIO_SECRET_KEY

      - name: cold-archive
        provider: s3
        endpoint: "https://minio.internal:9000"
        bucket: cold-backups
        prefix: "scripts/"
        mode: archive
        retain: 30              # obrigatório: quantos backups manter no bucket
        credentials:
          access_key_env: MINIO_ACCESS_KEY
          secret_key_env: MINIO_SECRET_KEY
```

> [!IMPORTANT]
> **Credenciais são referenciadas por variável de ambiente**, nunca hardcoded no YAML. Isso mantém segurança com secrets managers, systemd `EnvironmentFile`, etc.

---

## Arquitetura Proposta

```
                  ┌──────────────────┐
                  │ validateAndCommit │
                  │   (single/para)  │
                  └────────┬─────────┘
                           │
                    Commit (rename)
                           │
                    Rotate  (local)
                           │
              ┌────────────┴────────────┐
              │  PostCommitOrchestrator  │
              │  (goroutine por bucket)  │
              └────────────┬────────────┘
                ┌──────────┼──────────┐
                ▼          ▼          ▼
          ┌──────────┐ ┌──────────┐ ┌──────────┐
          │  sync    │ │ offload  │ │ archive  │
          │ (mirror) │ │ (push)   │ │ (trash)  │
          └──────────┘ └──────────┘ └──────────┘
```

### Fluxo por Modo

- **sync**: Upload do `finalPath` → log sucesso. Quando o Rotate local deleta backups antigos, o sync **também deleta no bucket** (espelhamento 1:1). Não bloqueia FinalACK.
- **offload**: Upload do `finalPath` → `os.Remove(finalPath)` → log. Aplica rotação no bucket via `retain` (bucket = storage primário). Bloqueia FinalACK (precisa garantir entrega).
- **archive**: Recebe lista de `removed` do Rotate → Upload de cada arquivo antes do delete local. Aplica `retain` no bucket para evitar crescimento infinito.

---

## Decisões de Design

### 1. Bloqueio vs. Assíncrono

| Modo      | FinalACK | Rotação no Bucket | Motivo |
|-----------|----------|-------------------|--------|
| `sync`    | Não bloqueia | Espelha o Rotate local (deleta mesmos arquivos) | Cópia 1:1; o local é a fonte de verdade. |
| `offload` | **Bloqueia** | Via `retain` (obrigatório) — bucket é o primário | Se upload falhar, local sobrevive. |
| `archive` | Não bloqueia | Via `retain` (obrigatório) | Evita crescimento infinito no bucket. |

> [!WARNING]
> No modo `offload`, se o upload falhar, o arquivo local **NÃO é deletado** e um `WARN` é emitido. O backup permanece íntegro localmente.

### 2. Rotação no Bucket

- **sync**: Não usa `retain`. Quando `Rotate()` local deleta um arquivo, o `PostCommitOrchestrator` recebe a lista de `removed` e deleta os mesmos objetos no bucket. Espelhamento total.
- **offload/archive**: Usam `retain`. Após cada upload, o orquestrador lista objetos no bucket (`Backend.List`), ordena por nome (timestamp) e deleta excedentes acima de `retain`.

### 3. Upload Paralelo

Cada bucket configurado roda em uma goroutine separada. Um `sync.WaitGroup` agrupa os modos que bloqueiam (`offload`). Os não-bloqueantes rodam em goroutines fire-and-forget com logging de erros.

### 4. Interface de Backend

```go
// ObjectStorageBackend define a interface para providers de object storage.
type ObjectStorageBackend interface {
    // Upload envia um arquivo local para o bucket.
    Upload(ctx context.Context, localPath, remotePath string) error
    // Delete remove um objeto do bucket.
    Delete(ctx context.Context, remotePath string) error
    // List lista objetos com prefixo no bucket (para retain do archive).
    List(ctx context.Context, prefix string) ([]string, error)
}
```

Começaremos com **S3-compatible** (cobre AWS S3, MinIO, Wasabi, DigitalOcean Spaces, etc.).

### 5. Retry & Resiliência

- Upload com retry exponencial (3 tentativas, backoff 1s→4s→16s).
- Context com timeout configurável (default: 30min por arquivo).
- Log detalhado de cada tentativa (slog structured).

---

## Proposed Changes

### Config

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

Adicionar structs `BucketConfig` e `BucketCredentials` ao pacote config. Adicionar campo `Buckets []BucketConfig` ao `StorageInfo`. Validar no `validate()`:
- `name` obrigatório e único dentro do storage
- `provider` deve ser `s3` (expandível)
- `mode` deve ser `sync`, `offload` ou `archive`
- `bucket` obrigatório
- `retain` obrigatório e > 0 quando mode = `offload` ou `archive`
- `retain` proibido quando mode = `sync` (espelha o local)
- credenciais: pelo menos `access_key_env` e `secret_key_env` preenchidos
- env vars referenciadas devem existir (warn, não erro fatal — podem ser injetadas depois)

---

### Object Storage Backend

#### [NEW] [objstore/](file:///home/lucas/Projects/n-backup/internal/objstore/)

Novo pacote `internal/objstore/` com:

#### [NEW] [backend.go](file:///home/lucas/Projects/n-backup/internal/objstore/backend.go)
- Interface `Backend` (Upload, Delete, List)
- Tipos auxiliares: `ObjectInfo`

#### [NEW] [s3.go](file:///home/lucas/Projects/n-backup/internal/objstore/s3.go)
- Implementação S3-compatible usando AWS SDK v2 (`github.com/aws/aws-sdk-go-v2`)
- Suporte a endpoint customizado (MinIO)
- Upload multipart para arquivos grandes
- Credenciais via env vars resolução dinâmica

#### [NEW] [s3_test.go](file:///home/lucas/Projects/n-backup/internal/objstore/s3_test.go)
- Testes com mock do S3 client (interface interna)

---

### Orquestrador Pós-Commit

#### [NEW] [post_commit.go](file:///home/lucas/Projects/n-backup/internal/server/post_commit.go)

- `PostCommitOrchestrator` struct que recebe `[]BucketConfig` e cria backends
- Método `Execute(ctx, finalPath, rotatedFiles, logger)` que orquestra goroutines
- Lógica de cada modo:
  - **sync**: Upload do `finalPath` + Delete dos `rotatedFiles` no bucket
  - **offload**: Upload do `finalPath` + `os.Remove` local + Rotate no bucket via `retain`
  - **archive**: Upload dos `rotatedFiles` + Rotate no bucket via `retain`
- WaitGroup para modos bloqueantes (`offload`)

#### [NEW] [post_commit_test.go](file:///home/lucas/Projects/n-backup/internal/server/post_commit_test.go)
- Testes com mock backend

---

### Hooks nos Handlers

#### [MODIFY] [handler_single.go](file:///home/lucas/Projects/n-backup/internal/server/handler_single.go)
- Em `validateAndCommit`: capturar `removed` do Rotate e chamar PostCommitOrchestrator **antes** do FinalACK (para `offload`) e em goroutine (para `sync`/`archive`).

#### [MODIFY] [handler_parallel.go](file:///home/lucas/Projects/n-backup/internal/server/handler_parallel.go)
- Em `validateAndCommitWithTrailer`: mesma integração.

---

### Configuração de Exemplo

#### [MODIFY] [server.example.yaml](file:///home/lucas/Projects/n-backup/configs/server.example.yaml)
- Adicionar seção `buckets` comentada com exemplos dos 3 modos.

---

### Documentação

#### [MODIFY] [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md)
- Seção sobre Object Storage pós-commit

#### [NEW] [post_commit_sync.puml](file:///home/lucas/Projects/n-backup/docs/diagrams/post_commit_sync.puml)
- Diagrama de sequência do fluxo pós-commit

---

## Dependências Externas

```
go get github.com/aws/aws-sdk-go-v2
go get github.com/aws/aws-sdk-go-v2/config
go get github.com/aws/aws-sdk-go-v2/service/s3
go get github.com/aws/aws-sdk-go-v2/credentials
```

> [!NOTE]
> O AWS SDK v2 é modular (só importa o necessário), tem zero deps transitivas pesadas e suporta qualquer endpoint S3-compatible.

---

## Verification Plan

### Testes Automatizados

```bash
# Testes unitários do pacote config (validação dos novos campos)
go test -race -v ./internal/config/ -run TestLoadServerConfig

# Testes unitários do pacote objstore (mock S3)
go test -race -v ./internal/objstore/

# Testes unitários do PostCommitOrchestrator (mock backend)
go test -race -v ./internal/server/ -run TestPostCommit

# Build completo
go build ./...
```

### Testes Manuais

> [!IMPORTANT]
> A integração real com S3/MinIO requer um ambiente com bucket configurado. Sugiro que o usuário valide com um MinIO local via Docker:
>
> ```bash
> docker run -d -p 9000:9000 -p 9001:9001 \
>   -e MINIO_ROOT_USER=minioadmin \
>   -e MINIO_ROOT_PASSWORD=minioadmin \
>   minio/minio server /data --console-address ":9001"
> ```
>
> E configure no YAML com `endpoint: "http://localhost:9000"`.
