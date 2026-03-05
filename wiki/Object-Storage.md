# Object Storage (Pós-Commit)

O **n-backup** permite enviar backups automaticamente para um ou mais destinos de Object Storage **S3-compatible** logo após o commit local. O processo é transparente para o agent — toda a configuração ocorre no server.

---

## ✨ Visão Geral

Após cada backup commitado com sucesso (rename atômico + rotação local), o server executa o **PostCommitOrchestrator** que processa cada bucket configurado em paralelo.

![Fluxo Post-Commit](https://uml.nishisan.dev/proxy?src=https://raw.githubusercontent.com/nishisan-dev/n-backup/refs/heads/main/docs/diagrams/post_commit_sync.puml)

---

## 🔄 Modos de Operação

| Modo | Comportamento | `retain` | Bloqueia FinalACK? |
|------|--------------|----------|---------------------|
| **sync** | Upload do backup + espelha deletes do Rotate local | ❌ Proibido | Não |
| **offload** | Upload do backup + apaga local (bucket vira primário) | ✅ Obrigatório | **Sim** |
| **archive** | Upload apenas de backups deletados pelo Rotate | ✅ Obrigatório | Não |

### sync

Espelha 1:1 o storage local no bucket. Quando o Rotate local deleta backups antigos, os objetos correspondentes são deletados do bucket.

- **Sem `retain`** — a política de rotação é a mesma do `max_backups` local.
- **Não bloqueia** o `FinalACK` ao agent. Se o upload falhar, apenas log de warning.
- **Ideal para**: disaster recovery — cópia exata em outro local.

### offload

O bucket se torna o storage primário. Após upload confirmado, o arquivo local é removido.

- **`retain` obrigatório** — controla quantos backups manter no bucket.
- **Bloqueia** o `FinalACK` ao agent até a confirmação do upload. Se o upload falhar, o arquivo local é preservado e um warning é logado.
- **Ideal para**: economizar disco local, storage em NAS via MinIO.

### archive

Envia para o bucket apenas os backups que seriam **deletados** pelo Rotate local (cold storage).

- **`retain` obrigatório** — controla quantos backups arquivados manter no bucket.
- **Não bloqueia** o `FinalACK`. Melhor esforço.
- **Ideal para**: compliance, auditoria, backups de longa retenção.

---

## ⚙️ Configuração

A seção `buckets` é adicionada dentro de cada `storage` no `server.yaml`:

```yaml
storages:
  scripts:
    base_dir: /var/backups/scripts
    max_backups: 5

    buckets:
      # Espelho S3 — cópia exata do storage local
      - name: s3-mirror
        provider: s3
        region: us-east-1
        bucket: my-backup-bucket
        prefix: "scripts/"
        mode: sync
        credentials:
          access_key_env: AWS_ACCESS_KEY_ID
          secret_key_env: AWS_SECRET_ACCESS_KEY

      # Offload para MinIO interno — bucket vira primário
      - name: offload-nas
        provider: s3
        endpoint: "https://minio.internal:9000"
        bucket: primary-backups
        prefix: "scripts/"
        mode: offload
        retain: 10
        credentials:
          access_key_env: MINIO_ACCESS_KEY
          secret_key_env: MINIO_SECRET_KEY

      # Archive — cold storage de backups deletados
      - name: cold-archive
        provider: s3
        endpoint: "https://minio.internal:9000"
        bucket: cold-backups
        prefix: "scripts/"
        mode: archive
        retain: 30
        credentials:
          access_key_env: MINIO_ACCESS_KEY
          secret_key_env: MINIO_SECRET_KEY
```

### Campos

| Campo | Obrigatório | Descrição |
|-------|-------------|-----------|
| `name` | ✅ | Nome único dentro do storage |
| `provider` | Não (default: `s3`) | Provider de object storage. Apenas `s3` suportado (compatível com AWS, MinIO, Wasabi, DO Spaces) |
| `endpoint` | Não | URL do endpoint. Vazio = AWS padrão. Preenchido = endpoint custom (MinIO, etc.) |
| `region` | Não (default: `us-east-1`) | Região AWS |
| `bucket` | ✅ | Nome do bucket |
| `prefix` | Não | Prefixo para objetos no bucket (ex: `scripts/`) |
| `mode` | ✅ | `sync`, `offload` ou `archive` |
| `retain` | Condicional | Obrigatório para `offload` e `archive`. Proibido para `sync` |
| `credentials.access_key_env` | ✅ | Nome da variável de ambiente com access key |
| `credentials.secret_key_env` | ✅ | Nome da variável de ambiente com secret key |

> [!IMPORTANT]
> Credenciais são referenciadas por **variáveis de ambiente** — nunca coloque valores diretamente no YAML.
> Configure as variáveis no systemd (`Environment=` ou `EnvironmentFile=`) ou no shell.

---

## 🔐 Segurança de Credenciais

```ini
# /etc/systemd/system/nbackup-server.service.d/override.conf
[Service]
Environment=AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE
Environment=AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
Environment=MINIO_ACCESS_KEY=minioadmin
Environment=MINIO_SECRET_KEY=minioadmin123
```

Alternativamente, use `EnvironmentFile=`:

```ini
[Service]
EnvironmentFile=/etc/nbackup/bucket-credentials.env
```

---

## 🔁 Mecanismo de Retry

Uploads utilizam **retry com exponential backoff**:

| Tentativa | Backoff |
|-----------|---------|
| 1 | 1s |
| 2 | 4s |
| 3 | 16s |

Máximo de **3 tentativas** por upload. Timeout por upload: **30 minutos**.

Se todas as tentativas falharem:
- **sync/archive**: Warning no log, `FinalACK` já enviado.
- **offload**: Erro logado, arquivo local **preservado**, `FinalACK` reporta sucesso (backup está safe localmente).

---

## 📊 Fluxo por Modo

### sync

```
Commit → Rotate → Upload(finalPath) → Delete(rotated files no bucket) → FinalACK OK
                                       ▲                                 ▲
                                       └── fire-and-forget               └── não espera
```

### offload

```
Commit → Rotate → Upload(finalPath) → Delete(local) → Rotate(bucket via retain) → FinalACK OK
                   ▲                                                                ▲
                   └── BLOQUEIA até confirmação ──────────────────────────────────────┘
```

### archive

```
Commit → Rotate → Upload(rotated files) → Rotate(bucket via retain) → FinalACK OK
                   ▲                                                    ▲
                   └── fire-and-forget                                  └── não espera
```

---

## 🏗️ Arquitetura Interna

| Componente | Arquivo | Responsabilidade |
|------------|---------|------------------|
| **PostCommitOrchestrator** | `internal/server/post_commit.go` | Orquestra operações por modo com goroutines paralelas |
| **Backend Interface** | `internal/objstore/backend.go` | `Upload`, `Delete`, `List` — abstração de provider |
| **S3Backend** | `internal/objstore/s3.go` | Implementação S3 via AWS SDK v2 (PutObject, DeleteObject, ListObjectsV2) |
| **Helper** | `internal/server/post_commit_helpers.go` | `runPostCommitSync` — resolve credenciais e invoca orquestrador |
| **SyncStorage** | `internal/server/sync_storage.go` | Sync retroativo: walk local → diff com remoto → upload faltantes |

---

## 🔄 Sync Retroativo (Backups Existentes)

Ao adicionar configuração de buckets `mode: sync` a um storage que **já possui backups locais**, os artefatos existentes não são sincronizados automaticamente — o Object Storage pós-commit só processa novos backups.

Para sincronizar backups pré-existentes, utilize o subcomando **`sync-storage`**:

### Uso

```bash
# Enviar sinal SIGUSR1 ao daemon por PID
nbackup-server sync-storage --pid <PID_DO_DAEMON>

# Ou via pid-file
nbackup-server sync-storage --pid-file /var/run/nbackup-server.pid
```

### Como Funciona

1. O CLI envia `SIGUSR1` ao processo do daemon.
2. O daemon recebe o sinal e inicia uma goroutine background de sync.
3. Para cada storage com buckets `mode: sync`:
   - Lista backups locais (`.tar.gz`, `.tar.zst`)
   - Lista objetos remotos no bucket
   - Faz upload dos arquivos que **não existem** no remoto
4. Logging detalhado de progresso e resultado final.

> [!IMPORTANT]
> O sync retroativo opera **exclusivamente** em buckets `mode: sync`.
> Modos `offload` e `archive` possuem semântica de lifecycle (deletar local / apenas rotated) que não faz sentido retroativamente.

### Características

- **Non-blocking**: o daemon continua processando backups normalmente durante o sync.
- **Guard atômico**: sinais duplicados são ignorados se um sync já estiver em execução.
- **Cancellation-safe**: respeita shutdown graceful (SIGTERM/SIGINT).
- **Retry**: uploads usam o mesmo mecanismo de retry com exponential backoff (3 tentativas).

### Verificação

Acompanhe o progresso nos logs do daemon:

```
INFO sync-storage: starting retroactive storage sync
INFO sync-storage: comparing local vs remote  storage=scripts  bucket=s3-mirror  local_count=12  remote_count=3
INFO sync-storage: uploaded successfully       file=agent1/daily/2026-01-15.tar.gz
INFO sync-storage: uploaded successfully       file=agent1/daily/2026-01-16.tar.gz
...
INFO sync-storage: completed  duration=2m30s  uploaded=9  skipped=3  errors=0
```

---

## ❓ FAQ

### Posso usar sync e archive no mesmo storage?
Sim. Cada bucket é processado de forma independente em goroutines paralelas.

### O que acontece se o bucket S3 estiver indisponível?
Depende do modo:
- **sync/archive**: Warning no log, backup local já está safe.
- **offload**: Upload falha → arquivo local é preservado → warning no log.

### Preciso instalar o AWS CLI?
Não. O n-backup usa o AWS SDK v2 nativamente em Go. Só precisa das variáveis de ambiente configuradas.

### Quais providers são compatíveis?
Qualquer endpoint S3-compatible: **AWS S3**, **MinIO**, **Wasabi**, **DigitalOcean Spaces**, **Backblaze B2** (modo S3), **Cloudflare R2**.

### Como sincronizar backups existentes após adicionar a configuração de buckets?
Use o subcomando `nbackup-server sync-storage --pid <PID>`. Veja a seção [Sync Retroativo](#-sync-retroativo-backups-existentes) acima.
