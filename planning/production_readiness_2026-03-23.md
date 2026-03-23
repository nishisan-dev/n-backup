# Production Readiness Report — n-backup

**Data:** 23/03/2026
**Revisão anterior:** [production_review.md](file:///home/lucas/Projects/n-backup/planning/production_review.md) (15/02/2026)
**Contexto:** Análise baseada em diagnóstico real de falha em produção (sessão `45d0567a`, popline-01, 22-23/03/2026)

## Status Atual

O projeto evoluiu significativamente desde a revisão de fevereiro. A arquitetura de protocolo, resiliência de rede e observabilidade estão em nível production-grade. Porém, o incidente de checksum mismatch de 22/03 expôs lacunas de **integridade de dados** e **gestão de lifecycle** que precisam ser fechadas antes de considerar o sistema GA.

**Avaliação:** Late Alpha → **Early Beta**

---

## Gaps Identificados para Production Level

### 🔴 P0 — Integridade de Dados

#### 1. Ausência de checksum por chunk

**Problema:** A única validação é o SHA-256 final (client vs server). Se um chunk corrompe em staging, a detecção ocorre horas depois, após o assembly completo.

**Evidência:** Sessão `45d0567a` — 435047 chunks recebidos corretamente, checksum mismatch detectado 9h depois no `finalizeLazy()`.

**Solução proposta:**
- Adicionar CRC32 ao `ChunkHeader` (4 bytes extras): o agent calcula antes de enviar, o server valida na recepção
- No lazy mode, persistir o CRC32 no nome do chunk file ou em metadata adjacente
- No `finalizeLazy()`, revalidar CRC32 antes de copiar para output
- Custo: ~negligível (CRC32 é hardware-accelerated em x86)

**Arquivos afetados:**
- `internal/protocol/chunk.go` — estender ChunkHeader com CRC32
- `internal/agent/dispatcher.go` — calcular CRC32 no `emitChunk()`
- `internal/server/handler_parallel.go` — validar CRC32 na recepção
- `internal/server/assembler.go` — revalidar no `finalizeLazy()`

#### 2. `chunk_fsync` desabilitado por padrão

**Problema:** Chunks em staging são escritos sem `f.Sync()`. Com 435k arquivos e assembly de 9h, page cache pressure pode causar corrupção silenciosa.

**Status:** Mitigado em 23/03 (habilitado manualmente no server popline). Precisa virar **default habilitado**.

**Solução proposta:**
- Alterar default de `FsyncChunkWrites` para `true` em `ChunkAssemblerOptions`
- Documentar trade-off de performance no `docs/configuration.md`

**Arquivos afetados:**
- `internal/server/handler_parallel.go` — default do `ChunkAssemblerOptions`
- `internal/config/server.go` — default na struct de config

---

### 🟡 P1 — Gestão de Lifecycle

#### 3. `MaxBackupDuration` compartilhado entre retries

**Problema:** O timeout de 24h é aplicado ao contexto do job, não por tentativa. Um retry após checksum mismatch herda o tempo restante, podendo ser morto imediatamente.

**Evidência:** Sessão `71697ffc` (retry) morta por `context deadline exceeded` às 00:05 porque o job original (`45d0567a`) consumiu as 24h.

**Solução proposta:**
- Mover o `context.WithTimeout(MaxBackupDuration)` para dentro de cada iteração do retry loop em `Scheduler.executeJob()`
- O timeout externo do cron (se houver) continua como safety net

**Arquivos afetados:**
- `internal/agent/scheduler.go` — reestruturar o timeout por tentativa
- `internal/agent/backup.go` — garantir que o ctx passado seja o do retry, não o do job

#### 4. Lazy assembler não escala

**Problema:** `finalizeLazy()` itera 435047 arquivos sequencialmente (`open → read → copy → remove`), levando 9h. Isso bloqueia o mutex do assembler durante todo o processo.

**Evidência:** Assembly de 08:04 a 17:12 (~9 horas) para 425 GB.

**Soluções possíveis (escopo de avaliação):**
- **Curto prazo:** Batch read com `readv`/`splice` ou IO paralelo no finalize
- **Médio prazo:** Assembly incremental no eager mode com `pendingMemLimit` aumentado (12-48MB cobre window real de 12 streams)
- **Longo prazo:** Streaming direto do ChunkBuffer para output ordenado (elimina staging)

**Arquivos afetados:**
- `internal/server/assembler.go` — `finalizeLazy()` e alternativas

---

### 🟡 P1 — Observabilidade e Diagnóstico

#### 5. Agent sem visibilidade da fase de assembly do server

**Problema:** Após `IngestionDone`, o agent fica idle esperando o `FinalACK`. Não sabe se o server está montando, validando ou com erro. A WebUI mostra streams "running" quando estão ociosos.

**Solução proposta:**
- Estender o ControlChannel com mensagem `AssemblyProgress` (server → agent)
- Agent expõe fase atual na WebUI e nos logs

#### 6. Session log não registra hash parciais

**Problema:** Quando o checksum falha, não há como determinar qual chunk causou a divergência.

**Solução proposta:**
- No `finalizeLazy()`, logar hash parcial a cada N chunks (ex: a cada 10000) para triangular a divergência
- Comparar com hash parcial do client (requer extensão do protocolo de Trailer)

---

### 🟢 P2 — Testes e Qualidade

#### 7. Testes de integração para cenários de falha

**Status desde fev/2026:** Cobertura ainda baixa em `internal/agent` (23.5%) e `internal/server` (15.9%)

**Cenários críticos sem cobertura:**
- Reconexão mid-transfer + lazy assembly + checksum validation
- ChunkBuffer backpressure + session abort + cleanup
- MaxBackupDuration timeout durante retry
- Port rotation + SACK timeout + retransmit

#### 8. Specification desatualizada

**Problema:** `docs/specification.md` não documenta ChunkBuffer, port rotation, flow rotation server-side, assembly phases na WebUI.

---

## Priorização Sugerida

| # | Item | Prioridade | Esforço | Risco se não feito |
|---|------|-----------|---------|---------------------|
| 1 | CRC32 por chunk | P0 | Médio | Corrupção silenciosa não detectada |
| 2 | `chunk_fsync` default true | P0 | Baixo | Corrupção em staging |
| 3 | MaxBackupDuration por retry | P1 | Baixo | Retries mortos prematuramente |
| 4 | Assembly progress no agent | P1 | Médio | Agent blind durante assembly |
| 5 | Hash parcial no finalize | P1 | Baixo | Diagnóstico impossível |
| 6 | Otimizar lazy assembler | P1 | Alto | Sessões de 9h+ de assembly |
| 7 | Testes de integração | P2 | Alto | Regressões em edge cases |
| 8 | Atualizar specification | P2 | Médio | Documentação inconsistente |

## Critérios para GA

- [ ] CRC32 por chunk implementado e validado
- [ ] `chunk_fsync` habilitado por padrão
- [ ] MaxBackupDuration independente por retry
- [ ] Assembly time < 1h para backups de 500 GB
- [ ] Testes de integração cobrindo cenários de reconexão + assembly
- [ ] Specification atualizada com features v3.x
- [ ] Zero checksum mismatches em 30 dias consecutivos de produção
