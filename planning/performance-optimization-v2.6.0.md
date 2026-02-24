# Otimizações de Performance — v2.5.3 → v2.6.0

Correção de regressão de performance identificada nos logs de produção. Três itens independentes: sharding configurável com cache de diretórios, reset de retry counter ao reconectar com sucesso, e verificação do binário deployed para o fix de `countBackups`.

## Proposed Changes

### Item 1 — Reset de Retry Counter no Dispatcher (Agent)

Os logs mostram um ciclo de reconexão contínuo (stream morre → reconecta → funciona → morre novamente). O dispatcher já possui backoff exponencial (1s→30s, max 5), mas **nunca reseta o counter `retries` após uma reconexão bem-sucedida seguida de I/O normal**. Isso significa que após 5 reconexões cumulativas ao longo de horas, o stream morre permanentemente — mesmo que cada reconexão individual tenha sido bem-sucedida.

> [!IMPORTANT]
> O fix é cirúrgico: a linha `retries = 0` (L389) está no lugar errado — é executada após o `conn.Write` bem-sucedido do primeiro pacote, mas está **dentro do bloco `if writeErr != nil`** (L297). O reset já existe, mas precisa ser movido para fora do bloco de erro.

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

- Mover `retries = 0` para **após** o `conn.Write` bem-sucedido, **fora** do bloco `if writeErr != nil`
- O reset deve acontecer em L388-392, quando um write é bem-sucedido (nenhum erro), confirmando que a conexão está saudável

---

### Item 2 — Sharding Configurável + Cache de MkdirAll (Server)

Dois sub-problemas:
1. **Sharding de 2 níveis é excessivo** — cria até 65536 subdirs; 1 nível (256 shards) é suficiente para a maioria dos cenários
2. **`os.MkdirAll` é chamado a cada chunk** — gera dezenas de milhares de syscalls `stat()` desnecessárias

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

- Adicionar campo `ChunkShardLevels int` ao `StorageInfo` com tag YAML `chunk_shard_levels`
- Default: `1` (256 shards), máximo: `2` (65536 shards)
- Validação no `validate()`: rejeitar valores < 1 ou > 2

#### [MODIFY] [assembler.go](file:///home/lucas/Projects/n-backup/internal/server/assembler.go)

- Adicionar campo `shardLevels int` ao `ChunkAssembler`
- Adicionar campo `ShardLevels int` ao `ChunkAssemblerOptions`
- Adicionar campo `createdShards map[string]bool` ao `ChunkAssembler` (cache de diretórios)
- Modificar `chunkPath()` para:
  - Usar 1 ou 2 níveis conforme `shardLevels`
  - Consultar `createdShards[shardDir]` antes de chamar `os.MkdirAll`
  - Armazenar em `createdShards` após criar com sucesso
- Remover constante `chunkShardFanout` (substituída por cálculo dinâmico)

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- Passar `storageInfo.ChunkShardLevels` para `ChunkAssemblerOptions` em `handleParallelBackup` (L1951-1953)

#### [MODIFY] [server.example.yaml](file:///home/lucas/Projects/n-backup/configs/server.example.yaml)

- Adicionar `chunk_shard_levels: 1` como exemplo comentado nos storages

---

### Item 3 — Verificação do fix countBackups

O fix de `countBackups` com `SkipDir` para `chunks_*` já foi implementado (commit `1c4ea64`), mas precisa ser verificado se o binário deployed em produção inclui este commit.

> [!NOTE]
> Este item não requer mudança de código — apenas verificação. Se o binário deployed não inclui o fix, basta fazer rebuild e redeploy da versão corrente.

---

## Verification Plan

### Automated Tests

**1. Testes existentes do assembler** (devem continuar passando — regressão):
```bash
cd /home/lucas/Projects/n-backup && go test ./internal/server/ -run TestChunkAssembler -v
```

**2. Testes existentes de config** (devem continuar passando):
```bash
cd /home/lucas/Projects/n-backup && go test ./internal/config/ -v
```

**3. Novos testes a adicionar:**

- `TestLoadServerConfig_ChunkShardLevels` — valida parsing do campo `chunk_shard_levels` (default 1, valores válidos 1-2, rejeição de inválidos)
- `TestChunkAssembler_SingleLevelSharding` — valida que com `ShardLevels: 1` o path tem apenas 1 nível (ex: `chunks_session/01/chunk_xxx.tmp`)
- `TestChunkAssembler_MkdirAllCache` — valida que dois chunks no mesmo shard geram apenas 1 chamada `MkdirAll` (verificável pela ausência de erro e existência do diretório)
- Atualizar `TestChunkAssembler_OutOfOrder_UsesShardedChunkPath` e `TestChunkAssembler_TwoLevelSharding_LargeSeq` para receber o novo parâmetro `ShardLevels`

**4. Teste completo com build:**
```bash
cd /home/lucas/Projects/n-backup && go test ./... -count=1
```
