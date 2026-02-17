# Code Review - Findings

Data: 2026-02-16
Escopo: revisão estática do projeto (Go), com execução de testes automatizados.

## Contexto da revisão

- Testes executados:
  - `go test ./...` ✅
  - `go test -race ./internal/agent ./internal/server` ✅
- Mesmo com testes verdes, foram encontrados riscos de confiabilidade e segurança não cobertos pelos cenários atuais.

## Findings

### 1. CRÍTICO - `panic` ao processar RESUME de sessão paralela (DoS do servidor)

- Evidência:
  - `internal/server/handler.go:981`
  - `internal/server/handler.go:987`
- Problema:
  - `handleResume` busca a sessão em `h.sessions` e faz type assertion direta para `*PartialSession` (`session := raw.(*PartialSession)`).
  - O mapa também armazena `*ParallelSession`.
  - Se chegar `RSME` com `sessionID` de sessão paralela ativa, ocorre `panic`.
- Impacto:
  - Queda total do processo (panic não recuperada), gerando negação de serviço.
- Recomendação:
  - Trocar assertion por type switch/check seguro e retornar `ResumeStatusNotFound` (ou status específico) para sessão incompatível.

### 2. ALTO - Path traversal em `agentName`/`backupName` permite escrita fora do `base_dir`

- Evidência:
  - `internal/server/handler.go:849`
  - `internal/server/handler.go:863`
  - `internal/server/storage.go:28`
- Problema:
  - `agentName` e `backupName` entram do protocolo sem sanitização.
  - Esses valores são usados em `filepath.Join(baseDir, agentName, backupName)`.
  - Nomes com `../` ou caminhos absolutos podem escapar de `baseDir`.
- Impacto:
  - Escrita/renomeação em caminhos indevidos no host do servidor (conforme permissões do processo).
- Recomendação:
  - Validar estritamente os identificadores (whitelist de caracteres e sem separador de path).
  - Além disso, verificar com `filepath.Clean` + `filepath.Rel`/prefix check que o destino final permanece dentro de `baseDir`.

### 3. ALTO - Identidade do agente não é vinculada ao certificado mTLS

- Evidência:
  - `internal/server/handler.go:849`
  - `internal/server/handler.go:869`
  - `internal/server/handler.go:813`
- Problema:
  - O nome do agente usado no backup vem do handshake (payload), não do CN/SAN do certificado cliente.
  - O CN é lido apenas no canal de controle.
- Impacto:
  - Um cliente com certificado válido da CA pode se passar por outro agente lógico e sobrescrever árvore de backup desse agente.
- Recomendação:
  - Vincular identidade do backup ao certificado (CN/SAN), ou no mínimo exigir igualdade entre `agentName` do protocolo e identidade do peer TLS.

### 4. ALTO - Roteamento incorreto de `ControlRotateACK` quando há múltiplas sessões do mesmo agente

- Evidência:
  - `internal/server/handler.go:764`
  - `internal/server/handler.go:775`
- Problema:
  - Ao receber `ControlRotateACK`, o loop em sessões para no primeiro `ParallelSession` do agente (`return false`) mesmo quando `RotatePending` daquele stream não existe naquela sessão.
- Impacto:
  - ACK pode ser consumido pela sessão errada ou descartado; rotação “graceful” cai em timeout e fallback abrupto desnecessário.
- Recomendação:
  - Continuar iterando até achar sessão com `RotatePending` correspondente.
  - Considerar incluir `sessionID` no frame de ACK para eliminar ambiguidade.

### 5. MÉDIO - Leitura de handshake sem limite de tamanho e sem deadline dedicado (risco de slowloris/OOM)

- Evidência:
  - `internal/server/handler.go:849`
  - `internal/server/handler.go:1249`
  - `internal/protocol/reader.go:291`
- Problema:
  - `readUntilNewline` acumula bytes indefinidamente até `\n`.
  - Parse por `ReadString('\n')` também é sem limite explícito.
  - Em partes do handshake não há deadline específico de leitura.
- Impacto:
  - Cliente malicioso/mal configurado pode manter goroutines presas e induzir consumo de memória.
- Recomendação:
  - Definir tamanho máximo por campo (ex.: agent/storage/backup/sessionID) e encerrar conexão ao exceder.
  - Aplicar read deadlines curtos durante handshake.

### 6. MÉDIO - `ChunkAssembler.Cleanup` não fecha/remover arquivo de saída temporário

- Evidência:
  - `internal/server/assembler.go:111`
  - `internal/server/assembler.go:401`
  - `internal/server/handler.go:1344`
- Problema:
  - O assembler abre `outFile` ao criar sessão.
  - Em cleanup (inclusive sessão expirada/falha), o método remove apenas `chunkDir`; não fecha `outFile` nem remove `outPath`.
- Impacto:
  - Vazamento de FD e acúmulo de `assembled_*.tmp` em cenários de erro/expiração.
- Recomendação:
  - No `Cleanup`, fechar `outFile` (se aberto) e remover `outPath` quando não finalizado/commitado.

### 7. MÉDIO - Data race em estado de stream do dispatcher (`active`/`dead`)

- Evidência:
  - Leitura: `internal/agent/dispatcher.go:204`
  - Escritas: `internal/agent/dispatcher.go:541`, `internal/agent/dispatcher.go:564`, `internal/agent/dispatcher.go:275`
- Problema:
  - Flags `active` e `dead` são lidas/escritas por múltiplas goroutines sem sincronização consistente (mutex/atomic únicos).
- Impacto:
  - Seleção inconsistente de stream e comportamento não determinístico sob carga/reconexão.
- Recomendação:
  - Centralizar acesso via mutex único do dispatcher ou usar campos atômicos para estado do stream.

### 8. MÉDIO - Data race em `BackupJob.LastResult`

- Evidência:
  - Escritas sem lock: `internal/agent/scheduler.go:116`, `internal/agent/scheduler.go:136`, `internal/agent/scheduler.go:163`, `internal/agent/scheduler.go:170`
  - Leitura concorrente com lock: `internal/agent/stats_reporter.go:100`
- Problema:
  - `LastResult` é atualizado fora de seção crítica em vários caminhos.
- Impacto:
  - Corrida de dados e snapshots inconsistentes no reporter.
- Recomendação:
  - Padronizar leitura/escrita de `LastResult` sob `job.mu` (ou usar `atomic.Pointer` com disciplina clara).

## Gaps de teste identificados

- Falta teste para `handleResume` com `sessionID` de `ParallelSession` (deveria negar sem `panic`).
- Falta teste de segurança para sanitização de `agentName`/`backupName` (path traversal).
- Falta teste de múltiplas sessões simultâneas do mesmo agente no roteamento de `ControlRotateACK`.
- Falta teste de limites de tamanho de campos de handshake e timeout de leitura no handshake.

