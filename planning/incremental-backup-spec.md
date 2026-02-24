# Especificação: Backup Incremental com Gerações

> **Status:** Proposta  
> **Data:** 2026-02-20  
> **Versão alvo:** v3.0.0

---

## 1. Motivação

O n-backup opera atualmente em modo **full backup** exclusivo: cada execução envia 100% dos dados elegíveis. Em cenários reais com servidores de **~500 GB e ~12 milhões de arquivos**, o delta diário é tipicamente inferior a **10 GB (~2%)**. O backup full diário resulta em:

- **~500 GB transferidos por dia** (98% redundante)
- **Tempo de transferência elevado** (proporcional ao volume total)
- **Storage multiplicado** pelo número de cópias retidas

A introdução de backup incremental reduz transferência e storage em ~98%, mantendo a capacidade de restauração consistente.

---

## 2. Conceitos Fundamentais

### 2.1 Geração (Generation)

Uma **geração** é uma unidade lógica de backup composta por:

- **1 backup full** (base da geração)
- **N backups incrementais** (deltas subsequentes)
- **N+1 manifests** (um por backup — full ou incremental)

A geração é a **unidade atômica de rotate**: quando o limite de retenção é atingido, a geração inteira é removida (full + todos os incrementais + manifests). Isso garante que nunca existam incrementais órfãos.

### 2.2 Manifest

O manifest é um **snapshot cumulativo** do estado completo do filesystem no momento do backup. Cada backup (full ou incremental) produz seu próprio manifest.

Características:

- **Cumulativo:** contém TODAS as entries (não apenas o delta) — a restauração de qualquer ponto requer leitura de **um único manifest**
- **Binário:** formato KV próprio com length-prefix, livre de problemas com paths contendo caracteres especiais
- **Comprimido:** envolvido em zstd streaming (~100 MB para 12M de entries)
- **Ordenado:** entries escritas na ordem lexicográfica do path (mesma ordem do `filepath.WalkDir`)
- **Origin-aware:** cada entry indica em qual archive (full ou incremental N) o arquivo está armazenado

### 2.3 Tipos de Backup

| Tipo | Quando | Conteúdo | Manifest |
|---|---|---|---|
| **Full** | Primeiro da geração | Todos os arquivos elegíveis | Snapshot completo, Origin=0 |
| **Incremental** | Demais dias da geração | Apenas arquivos novos ou modificados | Snapshot completo atualizado, Origin reflete archive de origem |

---

## 3. Formato do Manifest Binário

### 3.1 Header (40 bytes, fixo)

```
Offset  Tipo       Campo         Descrição
------  ---------  -----------   -----------------------------------------
0       [4]byte    Magic         "NBKM" (N-Backup Manifest)
4       uint16     Version       1
6       uint16     Flags         Bit 0: CUMULATIVE (sempre 1 nesta versão)
8       uint64     EntryCount    Número total de entries
16      int64      Timestamp     Unix nanosegundos do momento do backup
24      uint8      SeqNumber     0=full, 1=incr1, 2=incr2, ...
25      [15]byte   Reserved      Reservado para uso futuro
```

### 3.2 Entry (variável, ~103 bytes em média)

```
Offset  Tipo       Campo         Descrição
------  ---------  -----------   -----------------------------------------
0       uint16     PathLen       Comprimento do path em bytes (max 65535)
2       []byte     Path          Path relativo (UTF-8, sem null terminator)
2+N     int64      Mtime         Modificação em Unix nanosegundos
10+N    int64      Size          Tamanho em bytes
18+N    uint32     Mode          fs.FileMode (permissões + tipo)
22+N    uint8      Origin        Index do archive (0=full, 1=incr1, ...)
```

### 3.3 Footer (36 bytes, fixo)

```
Offset  Tipo       Campo         Descrição
------  ---------  -----------   -----------------------------------------
0       [32]byte   Checksum      SHA-256 de todo o conteúdo (header + entries)
32      [4]byte    Magic         "NBKM" (validação de integridade)
```

### 3.4 Estimativas de Tamanho (12M entries)

| Métrica | Valor |
|---|---|
| Tamanho raw | ~1.2 GB |
| Comprimido (zstd) | ~90-110 MB |
| Por geração (7 manifests) | ~700 MB |
| Overhead vs dados de backup (560 GB) | ~0.12% |

### 3.5 Armazenamento

O manifest é salvo como **sidecar** ao lado do archive `.tar.zst`:

```
gen-2026-02-14/
  2026-02-14T02-00-00.full.tar.zst
  2026-02-14T02-00-00.manifest.nbkm      ← manifest comprimido
  2026-02-15T02-00-00.incr.tar.zst
  2026-02-15T02-00-00.manifest.nbkm
  ...
```

A extensão `.nbkm` identifica o formato proprietário (N-Backup Manifest).

---

## 4. Detecção de Delta (Estratégia de Scan)

### 4.1 Merge-Join Streaming

A detecção de mudanças usa um **merge-join** entre dois streams ordenados:

1. **WalkDir** do filesystem (naturalmente ordenado pelo Go)
2. **ManifestReader** do manifest anterior (já ordenado por construção)

Ambos são lidos **sequencialmente, entry por entry**, resultando em:

- **O(n) tempo** (proporcional ao total de arquivos)
- **O(1) memória** (~200 bytes: a entry atual de cada stream)

### 4.2 Lógica de Classificação

```
Caso 1: walkPath < manifestPath  → ADICIONADO (arquivo novo)
Caso 2: walkPath > manifestPath  → DELETADO  (tombstone no manifest)
Caso 3: walkPath == manifestPath →
         Se mtime ou size diferem → MODIFICADO
         Senão                    → INALTERADO (skip)
```

- Arquivos **ADICIONADOS** e **MODIFICADOS** são incluídos no archive incremental
- Arquivos **DELETADOS** não são incluídos no archive, mas são **omitidos do novo manifest cumulativo**
- Arquivos **INALTERADOS** são copiados para o novo manifest cumulativo com o Origin do manifest anterior

### 4.3 Critério de Mudança

O critério padrão é `mtime + size`:

- **mtime:** `os.FileInfo.ModTime()` com precisão de nanosegundos
- **size:** `os.FileInfo.Size()`

Se ambos forem idênticos ao manifest anterior, o arquivo é considerado inalterado. Não há leitura de conteúdo — zero I/O adicional.

---

## 5. Estrutura de Storage

### 5.1 Layout de Diretórios

```
{baseDir}/
  {agentName}/
    {backupName}/
      gen-2026-02-14/                          ← Geração (diretório)
        2026-02-14T02-00-00.full.tar.zst         ← Full backup
        2026-02-14T02-00-00.manifest.nbkm        ← Manifest do full
        2026-02-15T02-00-00.incr.tar.zst         ← Incremental dia 2
        2026-02-15T02-00-00.manifest.nbkm        ← Manifest incremental
        2026-02-16T02-00-00.incr.tar.zst
        2026-02-16T02-00-00.manifest.nbkm
        ...
      gen-2026-02-21/                          ← Próxima geração
        2026-02-21T02-00-00.full.tar.zst
        ...
```

### 5.2 Nomenclatura dos Arquivos

| Tipo | Padrão |
|---|---|
| Full backup | `{timestamp}.full.tar.{ext}` |
| Incremental backup | `{timestamp}.incr.tar.{ext}` |
| Manifest | `{timestamp}.manifest.nbkm` |
| `ext` | `gz` ou `zst` conforme `compression_mode` |

### 5.3 Rotate por Geração

O **rotate** opera sobre gerações inteiras, não sobre arquivos individuais:

```go
// Pseudo-código
generations := listGenerationDirs(agentDir)  // ["gen-2026-02-07", "gen-2026-02-14", "gen-2026-02-21"]
sort.Strings(generations)

if len(generations) > maxGenerations {
    toRemove := generations[:len(generations)-maxGenerations]
    for _, gen := range toRemove {
        os.RemoveAll(filepath.Join(agentDir, gen))
    }
}
```

**Garantia:** é impossível ter um incremental órfão — o full e todos os incrementais vivem no mesmo diretório de geração.

---

## 6. Configuração

### 6.1 Princípio: Policy é do Server

A decisão sobre o tipo de backup (full ou incremental) é **responsabilidade exclusiva do server**. O server é o dono do storage — ele sabe quando foi o último full, quantas gerações existem, e quando é necessário iniciar uma nova geração.

O agent **não sabe e não precisa saber** se está fazendo full ou incremental. Ele conecta, o server informa o que fazer via handshake, e o agent executa.

Benefícios:

- **Zero ambiguidade:** `max_backups` vale para storages `type: full`, `max_generations` vale para `type: incremental`
- **Agent simplificado:** nenhum campo novo na config do agent
- **Política centralizada:** o admin configura uma vez no server, não em cada agent
- **Resiliência:** reinstalar o agent não perde contexto de ciclo — o server continua sabendo

### 6.2 Agent Config (zero mudanças)

```yaml
# agent.yaml — NENHUM campo novo para suportar incremental
backups:
  - name: "databases"
    storage: "incremental-store"   # aponta para o storage configurado no server
    schedule: "0 2 * * *"
    sources:
      - path: "/var/lib/postgresql"
    excludes:
      - "*.pid"
```

O agent apenas referencia o storage pelo nome. A política de backup (full/incremental) é definida no server.

### 6.3 Server Config (campos novos no storage)

```yaml
storages:
  # Storage tipo FULL (legado, default) — semântica inalterada
  main:
    base_dir: "/backups/main"
    max_backups: 10            # Contagem de arquivos individuais
    compression_mode: "zstd"
    assembler_mode: "eager"
    # type: "full"             # Default implícito — não precisa declarar

  # Storage tipo INCREMENTAL — campos novos
  incremental-store:
    base_dir: "/backups/incr"
    compression_mode: "zstd"
    assembler_mode: "eager"
    type: "incremental"        # Habilita modo incremental
    max_generations: 4         # Número máximo de gerações retidas
    full_interval: 7           # Dias entre full backups
```

| Campo | Tipo | Default | Contexto | Descrição |
|---|---|---|---|---|
| `type` | string | `"full"` | Storage | `"full"` ou `"incremental"` |
| `max_backups` | int | — | `type: full` | Número máximo de backups individuais |
| `max_generations` | int | — | `type: incremental` | Número máximo de gerações retidas |
| `full_interval` | int | `7` | `type: incremental` | Dias entre backups full |

> **Validação:** `max_backups` e `max_generations` são mutuamente exclusivos. O server rejeita config que defina ambos no mesmo storage.

---

## 7. Protocolo

### 7.1 Fluxo de Decisão (Server-Driven)

O **server** decide o tipo de backup durante o handshake. O fluxo é:

```
1. Agent conecta e envia handshake normal (agentName, storageName, backupName)
2. Server verifica o tipo do storage referenciado:
   - type=full  → responde com BackupType=FULL (comportamento atual)
   - type=incremental → server avalia:
     a. Existe geração ativa com full completo?
     b. O full_interval expirou?
     c. O manifest anterior existe e é válido?
     → Responde com BackupType=FULL ou BackupType=INCR
3. Agent recebe a decisão e age conforme
```

### 7.2 Handshake Response Estendido

O frame de resposta do handshake ganha campos novos:

```
Campo            Tipo     Descrição
-----------      ------   -----------------------------------------
BackupType       uint8    0x00=FULL, 0x01=INCR
ManifestReady    uint8    0x00=sem manifest, 0x01=manifest disponível
ManifestSize     uint64   Tamanho do manifest comprimido (bytes)
```

Quando `BackupType=INCR` e `ManifestReady=0x01`, o server envia o manifest anterior imediatamente após a resposta do handshake, antes do agent iniciar o scan. Isso permite ao agent fazer o merge-join sem depender de cópia local.

### 7.3 Transferência do Manifest (Agent → Server)

Após o backup ser comitado com sucesso, o agent envia o manifest novo:

```
1. Agent envia dados do backup (tar comprimido) → pipeline normal
2. Server monta, valida checksum, commit (rename .tmp → final)
3. Agent envia novo manifest comprimido como payload adicional
4. Server salva como sidecar (.manifest.nbkm) no diretório da geração
```

O manifest é gerado pelo agent durante o scan e transmitido ao server ao final da sessão. O server persiste o manifest e o utiliza para decisões de backups futuros.

### 7.4 Transmissão do Manifest (Server → Agent)

Para backups incrementais, o agent precisa do manifest anterior para o merge-join. O server é a **source of truth**:

```
1. Server respondeu BackupType=INCR, ManifestReady=0x01
2. Server transmite manifest comprimido (.nbkm) ao agent
3. Agent recebe via streaming e alimenta o ManifestReader
4. Agent inicia WalkDir + merge-join com o manifest recebido
```

**Fallback:** se o manifest não estiver disponível (`ManifestReady=0x00`), o server envia `BackupType=FULL` — o agent faz full automaticamente. Isso cobre cenários de primeira execução, corrupção de manifest, ou reinstalação do agent.

---

## 8. Restauração

### 8.1 Restore Completo (Latest)

```bash
nbackup restore --generation gen-2026-02-14 --target /restore/
```

1. Ler manifest mais recente da geração → construir set de entries esperadas
2. Extrair `full.tar.zst` no target
3. Extrair cada `incr-*.tar.zst` em ordem cronológica (sobrescreve/adiciona)
4. **Reconciliar:** walk do target, deletar qualquer arquivo ausente no manifest
5. Resultado: espelho fiel do source no ponto do último incremental

### 8.2 Restore por Data (Point-in-Time)

```bash
nbackup restore --generation gen-2026-02-14 --date 2026-02-16 --target /restore/
```

Mesmo processo, mas para no incremental de 2026-02-16 (não aplica incrementais posteriores).

### 8.3 Restore de Arquivo Específico

```bash
nbackup restore --generation gen-2026-02-14 --date 2026-02-16 \
    --file "/var/data/export.csv" --target /restore/
```

1. Ler manifest de 2026-02-16 → buscar entry pelo path
2. Campo `Origin` indica archive de origem (ex: `incr-day2`)
3. Extrair apenas esse arquivo do archive correto → **O(1)** sem abrir outros tars

### 8.4 Listagem (Auditoria)

```bash
nbackup restore --generation gen-2026-02-14 --date 2026-02-16 --list-only
```

Lê apenas o manifest (~100 MB comprimido) e lista os 12M de entries. Sem abrir nenhum tar.

---

## 9. Comportamento de Fallback e Edge Cases

| Cenário | Comportamento |
|---|---|
| **Primeiro backup (sem manifest anterior)** | Server envia `BackupType=FULL`, inicia nova geração |
| **Manifest anterior corrompido (checksum falha)** | Server loga warning, envia `BackupType=FULL`, nova geração |
| **Full falha no meio** | Geração não é criada (diretório é removido no abort) |
| **Incremental falha no meio** | Archive parcial removido, manifest não atualizado |
| **Storage sem `type` definido** | Assume `type: full` — 100% backward compatible |
| **Agent reinstalado** | Transparente — server possui manifest e decide normalmente |
| **`full_interval` excedido** | Server envia `BackupType=FULL`, inicia nova geração |
| **Agent aponta para storage `incremental` mas server é versão antiga** | Handshake sem `BackupType` → agent assume full (fallback) |

---

## 10. Backward Compatibility

- Storages existentes (sem `type`) operam como **full-only** — zero breaking change
- O campo `max_backups` permanece válido e exclusivo para storages `type: full`
- O campo `max_generations` é exclusivo para storages `type: incremental`
- Agent config não tem nenhum campo novo — apenas referencia o storage pelo nome existente
- Diretórios de geração (`gen-*`) coexistem com archives flat legados no mesmo base_dir
- Agents antigos (sem suporte a incremental) conectando a storages `type: incremental` recebem `BackupType=FULL` do server (graceful degradation)

---

## 11. Fases de Implementação Sugeridas

### Fase 1 — Manifest Engine

- `ManifestWriter`: escrita streaming binária com compressão zstd
- `ManifestReader`: leitura streaming entry-by-entry
- `ManifestInspect`: sub-comando CLI para debug/inspeção
- Testes unitários completos

### Fase 2 — Incremental Scanner

- `IncrementalScanner`: merge-join entre WalkDir e ManifestReader
- Geração do novo manifest cumulativo durante o scan
- Integração com pipeline existente do agent (streamer/dispatcher)
- Testes com cenários de add/modify/delete

### Fase 3 — Server Config e Storage

- Campos `type`, `max_generations`, `full_interval` no `StorageInfo`
- Validação: `max_backups` e `max_generations` mutuamente exclusivos
- Lógica de decisão no server: avaliar geração ativa, `full_interval`, manifest disponível
- Reestruturação do `AtomicWriter` para suportar diretórios de geração
- Nomenclatura `.full` / `.incr` nos archives
- Salvamento do manifest como sidecar no diretório da geração
- `Rotate` por geração (remove diretório inteiro)

### Fase 4 — Protocolo

- Campos `BackupType`, `ManifestReady`, `ManifestSize` no handshake response
- Transmissão do manifest anterior (server → agent) antes do scan
- Recepção do manifest novo (agent → server) após commit
- Graceful degradation para agents antigos

### Fase 5 — Restore

- Sub-comando `nbackup restore` no agent
- Restore completo, por data, por arquivo
- Reconciliação com tombstones via manifest
- `--list-only` para auditoria

### Fase 6 — WebUI e Observabilidade

- Indicação de tipo (full/incr) nas sessões
- Visualização de gerações
- Métricas: economia de storage, tamanho de delta, entries no manifest

---

## 12. Estimativas de Impacto

### Para o cenário de 500 GB / 12M arquivos:

| Métrica | Full-only (atual) | Incremental (proposto) |
|---|---|---|
| Transferência diária | ~500 GB | ~10 GB |
| Storage (4 gerações × 7 dias) | 4 × 500 GB = 2 TB | 4 × 560 GB ≈ 2.24 TB |
| Storage com rotate (mantém 4) | 2 TB | ~560 GB |
| Tempo de scan | 10-20 min | 10-20 min (mesmo) |
| Overhead do manifest | 0 | ~0.12% do storage |
| Economia de transferência | — | **~98%** |
