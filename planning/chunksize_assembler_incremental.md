# Fix ChunkSize + Assembler Incremental

O `ChunkSize` do Dispatcher está erroneamente vinculado ao `BufferSizeRaw` (256MB), e o `ChunkAssembler` no server cria um arquivo `.tmp` separado para cada chunk recebido — resultando em **54.000+ arquivos temporários** para um backup de ~14GB.

## Proposed Changes

### Config (Agent)

#### [MODIFY] [agent.go](file:///home/lucas/Projects/n-backup/internal/config/agent.go)
- Adicionar constante `DefaultChunkSize = 1 * 1024 * 1024` (1MB)
- Adicionar campo `ChunkSize string` e `ChunkSizeRaw int64` ao `ResumeConfig`
- Parsear e validar no `validate()` com default de 1MB

#### [MODIFY] [agent.example.yaml](file:///home/lucas/Projects/n-backup/configs/agent.example.yaml)
- Adicionar `chunk_size: 1mb` ao bloco `resume`

---

### Agent (Dispatcher + Backup)

#### [MODIFY] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)
- Linha 69: `chunkSize := uint32(cfg.Resume.ChunkSizeRaw)` (era `BufferSizeRaw`)
- Linha 349: `ChunkSize: int(cfg.Resume.ChunkSizeRaw)` (era `BufferSizeRaw`)

---

### Server (Assembler Incremental)

#### [MODIFY] [assembler.go](file:///home/lucas/Projects/n-backup/internal/server/assembler.go)

Reescrita completa do `ChunkAssembler` para **streaming incremental**:

- Em vez de criar 1 arquivo por chunk, mantém um **único arquivo `.tmp`** de saída
- Mantém contador `nextExpectedSeq` e um buffer de out-of-order (`pendingChunks map`)
- Novo método `WriteChunk(globalSeq uint32, data io.Reader, length int64)`:
  1. Se `globalSeq == nextExpectedSeq` → escreve direto no arquivo + flush pendentes em ordem
  2. Se `globalSeq > nextExpectedSeq` → bufferiza em arquivo temporário individual (out-of-order) 
  3. Após cada escrita in-order, verifica se há pending chunks contíguos e os descarrega
- Remove `ChunkFileForSeq` e `RegisterChunk` — substituídos por `WriteChunk`
- `Assemble()` não é mais necessário — substituído por `Finalize()` que retorna o path do arquivo já montado
- `Cleanup()` limpa apenas o diretório de chunks out-of-order (se houver)

> [!IMPORTANT]
> Com chunks em ordem (caso normal de single-stream ou round-robin sequencial), **zero arquivos temporários são criados**. Só há staging temporário quando chunks chegam fora de ordem em multi-stream.

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- `receiveParallelStream`: substituir `ChunkFileForSeq` + `RegisterChunk` por chamada ao novo `WriteChunk`
- `handleParallelBackup`: remover chamada a `Assemble()`, usar `Finalize()` para obter o path do arquivo pronto
- Remover `ChunkMeta` struct (não mais necessária)

---

### Testes

#### [MODIFY] [assembler_test.go](file:///home/lucas/Projects/n-backup/internal/server/assembler_test.go)
- Reescrever todos os testes para usar a nova API (`WriteChunk` / `Finalize`)
- Manter cenários: single-stream in-order, multi-stream in-order, out-of-order, cleanup

#### [MODIFY] [integration_test.go](file:///home/lucas/Projects/n-backup/internal/integration/integration_test.go)
- Atualizar teste `TestEndToEnd_ParallelBackup` para usar chunk size correto (1MB ou menor para teste)

---

### Documentação

#### [MODIFY] [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md)
- Atualizar seção do ChunkSize para refletir que é configurável (default 1MB)

#### [MODIFY] [usage.md](file:///home/lucas/Projects/n-backup/docs/usage.md)
- Adicionar `chunk_size` na tabela de configuração do resume

## Verification Plan

### Testes Automatizados

```bash
# Todos os testes unitários + integração
go test ./... -v -count=1

# Vet
go vet ./...
```

### Testes Manuais
- Deploy do server + agent com config atualizado e observar que:
  - Chunks são significativamente maiores (1MB em vez de 256KB)
  - O diretório `chunks_<session>` contém poucos ou zero arquivos (em vez de 54k+)
  - O backup finaliza corretamente com checksum válido
