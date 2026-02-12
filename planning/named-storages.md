# Named Storages — Modelo N:N

## Objetivo

Evoluir de **1 storage global** para **storages nomeados**, permitindo que cada agent defina N blocos de backup independentes, cada um direcionado a um storage específico no server.

**Antes:** `agent → server (1 storage)`
**Depois:** `agent → server (N storages por sessão)`

---

## Modelo de Dados

### Agent Config (novo)

```yaml
agent:
  name: "web-server-01"

backups:                           # ← plural, lista de blocos
  - name: "app"                    # ← identificador local
    storage: "scripts"             # ← nome do storage no server
    sources:
      - path: /app/scripts
    exclude:
      - "*.log"

  - name: "home"
    storage: "home-dirs"
    sources:
      - path: /home
    exclude:
      - ".git/**"
      - "node_modules/**"
```

### Server Config (novo)

```yaml
storages:                          # ← mapa de storages nomeados
  scripts:
    base_dir: /var/backups/scripts
    max_backups: 5

  home-dirs:
    base_dir: /var/backups/home
    max_backups: 10
```

### Protocolo — Handshake estendido

```
┌──────────┬──────┬──────────────────┬───────┬───────────────────┬───────┐
│ "NBKP"   │ Ver  │ AgentName (UTF8) │ '\n'  │ StorageName (UTF8)│ '\n'  │
│ 4 bytes  │ 1B   │ variável         │ 1B    │ variável          │ 1B    │
└──────────┴──────┴──────────────────┴───────┴───────────────────┴───────┘
```

O ACK ganha novo status `StatusStorageNotFound = 0x04`.

---

## Proposed Changes

### Config

#### [MODIFY] [agent.go](file:///home/lucas/Projects/n-backup/internal/config/agent.go)

- Remover `BackupInfo` e `BackupSource` legados
- Criar `BackupEntry` com: `Name`, `Storage`, `Sources []BackupSource`, `Exclude []string`
- `AgentConfig.Backups []BackupEntry` (substitui `Backup BackupInfo`)
- Atualizar validação: cada entry deve ter `name`, `storage`, e pelo menos um source

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

- `Storage StorageInfo` → `Storages map[string]StorageInfo`
- Atualizar validação: pelo menos 1 storage, cada um com `base_dir` válido
- Adicionar método `GetStorage(name string) (*StorageInfo, bool)`

#### [MODIFY] [agent.example.yaml](file:///home/lucas/Projects/n-backup/configs/agent.example.yaml)

- Atualizar para formato `backups:` com lista de blocos nomeados

#### [MODIFY] [server.example.yaml](file:///home/lucas/Projects/n-backup/configs/server.example.yaml)

- Atualizar para formato `storages:` com mapa nomeado

---

### Protocolo

#### [MODIFY] [frames.go](file:///home/lucas/Projects/n-backup/internal/protocol/frames.go)

- Adicionar `StorageName string` ao struct `Handshake`
- Adicionar `StatusStorageNotFound byte = 0x04`

#### [MODIFY] [writer.go](file:///home/lucas/Projects/n-backup/internal/protocol/writer.go)

- `WriteHandshake(w, agentName, storageName)` — escrever storage name + `\n`

#### [MODIFY] [reader.go](file:///home/lucas/Projects/n-backup/internal/protocol/reader.go)

- `ReadHandshake` lê segundo campo (storage name) até `\n`

---

### Agent

#### [MODIFY] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

- `RunBackup` recebe `BackupEntry` em vez de `*AgentConfig` inteiro
- Passa storage name no handshake
- Usa sources/excludes do `BackupEntry`

#### [MODIFY] [daemon.go](file:///home/lucas/Projects/n-backup/internal/agent/daemon.go)

- `RunBackupWithRetry` itera sobre `cfg.Backups[]`, executando cada bloco sequencialmente
- Log identifica qual backup entry está rodando

---

### Server

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- Após ler storage name do handshake, busca em `cfg.Storages[storageName]`
- Se não encontrar → responde `StatusStorageNotFound`
- Lock key muda de `agentName` para `agentName:storageName` (backups diferentes do mesmo agent podem rodar em paralelo)
- `AtomicWriter` usa `storage.BaseDir` + `agentName/` como diretório
- Rotação usa `storage.MaxBackups`

---

### Testes

#### [MODIFY] [config_test.go](file:///home/lucas/Projects/n-backup/internal/config/config_test.go)

- Atualizar fixtures YAML para novo formato (backups/storages)

#### [MODIFY] [protocol_test.go](file:///home/lucas/Projects/n-backup/internal/protocol/protocol_test.go)

- Handshake round-trip agora inclui storage name

#### [MODIFY] [agent_test.go](file:///home/lucas/Projects/n-backup/internal/agent/agent_test.go)

- Sem mudanças (scanner/streamer não mudam)

#### [MODIFY] [server_test.go](file:///home/lucas/Projects/n-backup/internal/server/server_test.go)

- Sem mudanças (AtomicWriter/Rotate não mudam)

#### [MODIFY] [integration_test.go](file:///home/lucas/Projects/n-backup/internal/integration/integration_test.go)

- Handshake agora envia storage name
- Server config usa `Storages` map
- Novo teste: `StorageNotFound` → agent envia storage inválido → ACK 0x04

---

### Documentação

#### [MODIFY] [usage.md](file:///home/lucas/Projects/n-backup/docs/usage.md)
#### [MODIFY] [installation.md](file:///home/lucas/Projects/n-backup/docs/installation.md)
#### [MODIFY] [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md)

- Atualizar exemplos de config e protocolo

---

## Verification Plan

### Automated Tests
```bash
go test ./... -v -count=1
```
- Config: validação dos novos formatos YAML
- Protocol: round-trip com storage name
- Integration: fluxo E2E com storage nomeado + teste de storage inexistente

### Manual Verification
- `go build ./cmd/nbackup-agent ./cmd/nbackup-server` — binários compilam
