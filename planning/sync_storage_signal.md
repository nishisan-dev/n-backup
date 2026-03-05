# Mecanismo de Sinalização CLI → Daemon para Sync de Storage com Object Storage

## Descrição do Problema

O sistema n-backup agora suporta Object Storage pós-commit (sync/offload/archive), mas esse mecanismo só opera sobre **novos backups** recebidos após a configuração dos buckets. Se o operador adicionar a configuração de buckets a um storage que já possui backups locais, esses artefatos existentes **nunca serão sincronizados** automaticamente.

A solução proposta usa um **sinal Unix (SIGUSR1)** para que o CLI possa instruir o daemon em execução a iniciar uma sincronização retroativa — sem bloquear o daemon nem exigir um comando CLI de longa duração.

## Decisões de Design

### Por que SIGUSR1?

| Alternativa         | Prós                          | Contras                                                  |
|:-------------------:|:-----------------------------:|:---------------------------------------------------------:|
| **SIGUSR1**         | Zero config, zero overhead, nativo Unix | Sem payload (não especifica *qual* storage)      |
| HTTP endpoint       | Pode receber parâmetros       | Requer WebUI habilitada, abre superfície de ataque       |
| Unix socket         | Payload flexível              | Complexidade adicional, novo arquivo de socket            |
| Named pipe (FIFO)   | Simples                       | Gerenciamento de lifecycle, limpeza em crash              |

**Decisão**: SIGUSR1 é a abordagem mais idiomática para daemons Go, zero overhead, e resolve o problema sem dependências. O tradeoff de não ter payload é aceitável porque o comportamento desejado é **sincronizar todos os storages com buckets configurados**.

### Filosofia do Sync Retroativo

O sync retroativo opera **exclusivamente no modo `sync`** (espelhamento 1:1). Modos `offload` e `archive` possuem semântica de data lifecycle (deletar local / apenas rotated) que não fazem sentido para artefatos retroativos.

> [!IMPORTANT]
> O fluxo foca apenas em `mode: sync`. Backups existentes no modo `offload` seriam deletados localmente após upload, o que seria destrutivo sem confirmação explícita. O modo `archive` só faz sentido para arquivos sendo rotacionados.

---

## Proposed Changes

### Componente: Signal Handler

#### [MODIFY] [main.go](file:///home/lucas/Projects/n-backup/cmd/nbackup-server/main.go)

Adicionar suporte a subcomando `sync-storage`:

- Antes de `flag.Parse()`, checar se `os.Args[1] == "sync-storage"`, ler PID do daemone enviar `SIGUSR1` via `syscall.Kill`.
- Se `--pid-file` for informado, ler PID do arquivo; caso contrário, exigir `--pid <PID>`.

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/server/server.go)

Na função `Run`:

- Adicionar listen de `syscall.SIGUSR1` num channel dedicado (separado do sigCh de shutdown).
- Quando receber SIGUSR1, invocar `handler.SyncExistingStorage(ctx)` numa goroutine background.
- Usar um `sync.Once` (ou flag atômico) para evitar execuções concorrentes do sync.

---

### Componente: Sync Retroativo (Core)

#### [NEW] [sync_storage.go](file:///home/lucas/Projects/n-backup/internal/server/sync_storage.go)

Novo arquivo com a lógica de sincronização retroativa:

```go
// SyncExistingStorage percorre todos os storages configurados que possuem
// buckets no modo sync, e faz upload dos backups locais que não existem
// no bucket remoto.
func (h *Handler) SyncExistingStorage(ctx context.Context)
```

**Algoritmo**:
1. Iterar sobre todos os `cfg.Storages` que possuem `Buckets` configurados.
2. Filtrar apenas buckets com `Mode == "sync"`.
3. Para cada par (storage, bucket_sync):
   a. Instanciar o `objstore.Backend` via `defaultBackendFactory`.
   b. Listar backups locais no `BaseDir` (recursivo, apenas `*.tar.gz` e `*.tar.zst`).
   c. Listar objetos remotos via `backend.List(ctx, prefix)`.
   d. Calcular diff: backups locais que **não existem** no remoto (baseado no nome do arquivo).
   e. Upload dos faltantes com `uploadWithRetry` (reuso do `PostCommitOrchestrator`).
4. Emitir eventos para a WebUI (`Events.PushEvent`) sobre cada upload e o sumário final.
5. Logging detalhado de início/fim, quantidade de arquivos processados, uploads, erros.

```go
// syncOneBucket sincroniza um storage+bucket específico.
func (h *Handler) syncOneBucket(ctx context.Context, storageName string, si config.StorageInfo, bcfg config.BucketConfig, logger *slog.Logger) (uploaded int, skipped int, errors int)
```

**Considerações**:
- **Concorrência**: os storages/buckets são processados sequencialmente para não sobrecarregar o IO/rede.
- **Guard atômico**: `Handler` ganha um campo `syncRunning atomic.Bool` para evitar sync concorrente.
- **Cancelamento**: respeita `ctx.Done()` para shutdown graceful durante sync.

---

#### [NEW] [sync_storage_test.go](file:///home/lucas/Projects/n-backup/internal/server/sync_storage_test.go)

Testes unitários cobrindo:

1. **TestSyncStorage_UploadsM missingFiles**: cria diretório com 3 backups locais, mock retorna apenas 1 remoto → verifica que os 2 faltantes foram uploaded.
2. **TestSyncStorage_SkipsExisting**: todos locais já existem no remoto → nenhum upload.
3. **TestSyncStorage_OnlySyncMode**: config com bucket offload e sync → verifica que apenas sync é processado.
4. **TestSyncStorage_NoBackups**: storage sem backups locais → nenhum upload.
5. **TestSyncStorage_GuardConcurrent**: verifica que duas invocações simultâneas resultam em apenas uma execução.
6. **TestSyncStorage_RespectsContextCancel**: ctx cancelado antes do loop → nenhum upload.

---

### Componente: CLI (subcomando)

#### [MODIFY] [main.go](file:///home/lucas/Projects/n-backup/cmd/nbackup-server/main.go)

Adicionar subcomando `sync-storage` que:
1. Lê o PID do daemon: flag `--pid <PID>` ou `--pid-file <path>`.
2. Envia `syscall.SIGUSR1` ao PID.
3. Imprime confirmação no stdout.

```
nbackup-server sync-storage --pid 12345
nbackup-server sync-storage --pid-file /var/run/nbackup-server.pid
```

---

### Componente: Handler (campo atomico)

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

Adicionar campo ao `Handler`:

```go
// syncRunning guarda o estado do sync retroativo para evitar execuções concorrentes.
syncRunning atomic.Bool
```

---

## Diagrama de Sequência

```
┌──────────┐        ┌──────────────┐        ┌─────────┐        ┌──────┐
│ CLI      │        │ Daemon       │        │ Handler │        │  S3  │
│ (server) │        │ (server.Run) │        │         │        │      │
└────┬─────┘        └──────┬───────┘        └────┬────┘        └──┬───┘
     │ SIGUSR1             │                      │                │
     │─────────────────────>                      │                │
     │                     │ SyncExistingStorage() │                │
     │                     │──────────────────────>│                │
     │                     │                      │ List(prefix)   │
     │                     │                      │───────────────>│
     │                     │                      │<───────────────│
     │                     │                      │ Upload(missing)│
     │                     │                      │───────────────>│
     │                     │                      │<───────────────│
     │                     │<──────────────────────│ done           │
     │                     │                      │                │
```

---

## Verification Plan

### Testes Automatizados

```bash
# Roda TODOS os testes do projeto (garante não-regressão)
cd /home/lucas/Projects/n-backup && go test ./... -count=1 -race

# Build (garante compilação)  
cd /home/lucas/Projects/n-backup && go build ./...

# Vet (garante qualidade)
cd /home/lucas/Projects/n-backup && go vet ./...
```

### Testes Específicos do Sync

```bash
# Apenas os testes do sync_storage
cd /home/lucas/Projects/n-backup && go test ./internal/server/ -run TestSync -v -race
```

### Verificação Manual (pelo usuário)

1. Compilar o binário: `go build -o ./nbackup-server ./cmd/nbackup-server/`
2. Verificar help: `./nbackup-server sync-storage --help` → deve exibir flags `--pid` e `--pid-file`
3. Com o daemon rodando, executar: `./nbackup-server sync-storage --pid <PID_DO_DAEMON>`
4. Verificar nos logs do daemon se o sync foi iniciado e concluído
