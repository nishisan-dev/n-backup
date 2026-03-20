# Changelog

Todas as mudanças notáveis do projeto são documentadas neste arquivo.

O formato é baseado em [Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/)
e o versionamento segue [Semantic Versioning](https://semver.org/lang/pt-BR/).

---

## [Unreleased]

### Corrigido
- **Sessões expiradas registradas no histórico**: o `CleanupExpiredSessions` agora chama `recordSessionEnd` com resultado `expired` antes de remover sessões parciais e paralelas expiradas. Sessões que antes desapareciam silenciosamente agora são visíveis no Session History do dashboard com badge `expired`.
- **Evento `session_expired`**: emitido no EventStore quando uma sessão é removida por expiração, incluindo nome do agent, storage, backup e tempo idle. Visível na aba Events do WebUI.



## [v3.3.4] — 2026-03-06

Upload resiliente com stall detection — substitui timeout fixo de 30min por detecção de inatividade inteligente.

### Adicionado
- **Stall Detection para uploads S3**: o `S3Backend.Upload` agora wrapa o `io.Reader` do arquivo com um `stallDetectReader` que reseta um timer a cada `Read()` bem-sucedido. **Enquanto bytes estiverem fluindo, o upload continua indefinidamente** — sem deadline global. Se a conexão ficar parada por 5 minutos (configurável via `stall_timeout`), o contexto é cancelado e o retry é acionado. Resolve uploads de arquivos grandes (>100GB) que falhavam sistematicamente com `context deadline exceeded` após 30min.
- **Limpeza de multipart uploads incompletos**: novo método `AbortIncompleteUploads` na interface `Backend`. Após falha definitiva de upload, multipart uploads órfãos são abortados via `ListMultipartUploads` + `AbortMultipartUpload`, eliminando "pastas fantasmas" no bucket.
- **`stall_timeout` configurável por bucket**: campo `stall_timeout` no `BucketConfig` (YAML) permite ajustar a tolerância de inatividade por bucket (default: 5 minutos).
- **Logs de upload enriquecidos**: `size_human` (ex: `425.64 GB`), `avg_mbps`, `stall_timeout` nos logs de início/fim de upload para diagnóstico operacional.

### Removido
- **Timeout fixo de 30min**: o `context.WithTimeout` de 30 minutos em `uploadWithRetry` e `uploadWithRetryStandalone` foi completamente removido. O campo `timeout` do `PostCommitOrchestrator` também foi eliminado.

### Motivação
> Em produção (Contabo S3-compatible), uploads de backups de ~456GB falhavam após exatamente 30 minutos em cada tentativa (3 retries × 30min = ~3h desperdiçadas). O timeout fixo não considerava se dados ainda estavam fluindo — matava o upload mesmo com banda ativa. O stall detection resolve isso: se a conexão está lenta mas funcional (ex: 5MB/s), o upload continua por 25h se necessário. Só cancela quando a conexão realmente morre.

---

## [v3.3.3] — 2026-03-05

Sync jobs de Object Storage visíveis no WebUI com progresso em tempo real.

### Adicionado
- **Sync Jobs no WebUI (Overview)**: card "☁ Object Storage Sync" no dashboard com três estados:
  - **Durante sync**: barra de progresso animada, arquivo/bucket atual, ETA, contadores (uploaded/skipped/errors/bytes transferidos).
  - **Após conclusão**: resumo do último sync (duração, totais) com tabela detalhada por bucket.
  - **Idle**: card oculto automaticamente quando não há sync recente.
  - Integrado ao polling de 2s do dashboard — atualização em tempo real sem refresh manual.
- **API Endpoint `GET /api/v1/sync/status`**: retorna estado completo do sync retroativo (running, progresso em tempo real, último resultado com detalhes por bucket).
- **`SyncProgress` (contadores atômicos)**: struct com `atomic.Int64`/`atomic.Bool`/`atomic.Value` atualizada a cada arquivo processado em `syncOneBucket`. Leitura lock-free pelo endpoint HTTP — zero contention.
- **Métricas Prometheus** (5 novas):
  - `nbackup_server_sync_running` — sync ativo (0/1)
  - `nbackup_server_sync_files_uploaded_total`
  - `nbackup_server_sync_files_skipped_total`
  - `nbackup_server_sync_files_errors_total`
  - `nbackup_server_sync_bytes_uploaded_total`
- **DTOs**: `SyncStatusDTO`, `SyncProgressDTO`, `SyncResultDTO`, `SyncBucketDTO` no pacote `observability`.
- **3 testes unitários**: `TestSyncStatus_Idle`, `TestSyncStatus_Running`, `TestSyncStatus_WithLastResult`.

---

## [v3.3.2] — 2026-03-05

Fix de upload para arquivos grandes — suporte a multipart upload automático.

### Corrigido
- **`EntityTooLarge` em backups grandes**: o `S3Backend.Upload` usava `PutObject` simples, limitado a ~5GB por arquivo. Migrado para `s3manager.Uploader` (AWS SDK v2) que automaticamente faz multipart upload, dividindo o arquivo em parts de 64MB com 5 uploads paralelos. Resolve uploads de backups de centenas de GB (testado com arquivo de 456GB).

### Adicionado
- Dependência `github.com/aws/aws-sdk-go-v2/feature/s3/manager` para multipart upload automático.
- Log field `multipart: true/false` no início de cada upload para visibilidade.
- Log field `size` no final do upload para acompanhamento.

---

## [v3.3.1] — 2026-03-05

Sincronização retroativa de backups existentes com Object Storage via sinalização CLI → Daemon.

### Adicionado
- **Sync Retroativo (`sync-storage`)**: novo subcomando `nbackup-server sync-storage --pid <PID>` que envia `SIGUSR1` ao daemon para triggerar upload de backups locais pré-existentes para buckets `mode: sync`. Resolve o cenário onde o operador adiciona configuração de Object Storage após já possuir backups locais.
  - Novo arquivo `internal/server/sync_storage.go` com `SyncExistingStorage()` — percorre storages, lista backups locais vs remotos, e faz upload dos faltantes.
  - Structs observáveis (`SyncStorageResult`, `SyncBucketResult`, `SyncStorageTotals`) JSON-serializáveis para futuro uso na WebUI.
  - Guard atômico (`syncRunning`) evita execuções concorrentes.
  - Último resultado armazenado em `lastSyncResult` para consulta futura via API.
  - Listener `SIGUSR1` integrado ao daemon (`server.go`) com dispatching para goroutine background.
  - Suporte a `--pid` e `--pid-file` no subcomando CLI.
  - 15 testes unitários com `-race` cobrindo: uploads faltantes, skip de existentes, filtro mode sync, guard concorrente, cancelamento de context, erro de upload, múltiplos storages e `.tar.zst`.
- **`defaultBackendFactory` testável**: transformada em variável de função (`var`) para permitir substituição em testes.

### Motivação
> O Object Storage pós-commit só processa **novos** backups. Se o operador adicionar a configuração de buckets a um storage que já possui backups locais, esses artefatos nunca seriam sincronizados automaticamente. O `sync-storage` resolve isso sem bloquear o daemon e sem exigir um comando CLI de longa duração — o daemon processa o sync em background enquanto continua atendendo backups normalmente.

---

## [v3.3.0] — 2026-03-04

Verificação de integridade de archives antes da rotação de backups (fail-safe).

### Adicionado
- **Verificação de Integridade (`verify_integrity`)**: nova opção booleana por storage entry (default: `false`) que valida a integridade de archives comprimidos (`.tar.gz` / `.tar.zst`) **após o commit e antes da rotação**. Se o archive recém-commitado estiver corrompido, a rotação é **abortada** — impedindo que backups antigos válidos sejam deletados em favor de um backup corrompido.
  - Verificação equivalente a `tar -I zstd -tf archive.tar.zst > /dev/null`: lê e descomprime todo o tarball, validando headers e integridade dos dados.
  - Novo arquivo `internal/server/integrity.go` com a função `VerifyArchiveIntegrity()`.
  - 9 testes unitários (`integrity_test.go`) cobrindo cenários: archive válido (gzip/zstd), corrompido, truncado, vazio, extensão não suportada e arquivo inexistente.
  - Integrado nos handlers `handler_single.go` e `handler_parallel.go` entre o Commit e o Rotate.
  - Configuração documentada em `configs/server.example.yaml`, `docs/specification.md`, `docs/usage.md` e `wiki/Especificacao-Tecnica.md`.

### Motivação
> Em cenários onde o disco de destino apresenta falhas silenciosas (bit rot, filesystem corrompido) ou a compressão falha por falta de memória, o backup commitado pode estar corrompido sem que o sistema detecte. Sem verificação, a rotação deletaria os backups antigos válidos, deixando apenas o backup corrompido. O `verify_integrity` atua como fail-safe: se a verificação falhar, os backups antigos são preservados.

---

## [v3.2.0] — 2026-03-02

Object Storage post-commit, melhorias de qualidade de código e fix de race condition.

### Adicionado
- **Object Storage Post-Commit Sync**: após o assembly bem-sucedido de um backup, o arquivo final pode ser sincronizado automaticamente para um bucket S3-compatível (AWS S3, MinIO, etc.). Configuração via `bucket` por storage entry com suporte a `endpoint`, `region`, `bucket`, `prefix` e credenciais via variáveis de ambiente. Implementado como `PostCommitOrchestrator` que se integra nos hooks de `validateAndCommit`.
  - Interface `Backend` extensível para futuros provedores de object storage.
  - Implementação `S3Backend` usando AWS SDK v2 com upload multipart automático.
  - Validação de configuração de bucket no startup.
- **CI: Race Detection e Linting**: pipeline de CI (`ci.yml`) agora inclui `go test -race`, `golangci-lint` e threshold de cobertura de testes.
- **CI: Wiki Sync Automático**: workflow (`wiki-sync.yml`) sincroniza automaticamente o diretório `wiki/` para o repositório GitHub Wiki em pushes na `main`.
- **Testes unitários para Autoscaler e Backup**: cobertura de testes para `autoscaler.go` e `backup.go` incluindo modos efficiency/adaptive, edge cases e thread safety.

### Alterado
- **Refactoring de `handler.go`**: arquivo monolítico do server splitado em 6 arquivos domain-specific (`handler_parallel.go`, `handler_control.go`, `handler_assembly.go`, `handler_observability.go`, `handler_session.go`, `handler_single.go`), melhorando navegabilidade e manutenção.

### Corrigido
- **Race condition archive vs Rotate**: corrigida condição de corrida entre a rotação de backups antigos e a escrita do arquivo final do assembler, que podia causar remoção prematura do backup recém-concluído.
- **Compatibilidade golangci-lint com Go 1.25.6**: ajuste de versão do linter para compatibilidade com a versão do Go em uso.

---

## [v3.1.0] — 2026-03-01

Release estável da linha v3.x. Resolve a causa raiz de chunks faltantes em shutdowns prematuros.

### Adicionado
- **Final ChunkSACK Drain**: o agent agora aguarda `rb.Tail() == rb.Head()` (confirmação real do server via `ChunkSACK`) em todos os streams **antes** de enviar `ControlIngestionDone`. Anteriormente, o sender declarava sucesso quando `conn.Write()` retornava — sem garantia de que o server realmente recebeu todos os bytes. Se o drain não completar no timeout (baseado em RTT × SACK timeout), o agent reconecta, revalida o offset e retransmite os bytes pendentes. Se max retries for excedido, o backup aborta explicitamente no agent, em vez de produzir gaps silenciosos no server.
- **Helper `HasUnackedData()`**: indica se um `ParallelStream` ainda possui bytes escritos mas não confirmados por `ChunkSACK`.
- **`waitForFinalDrain()`**: loop de espera com reconnect e retry para drenagem completa antes do shutdown.
- **`syncStreamAfterReconnect()`**: validação de alinhamento de `ChunkHeader` no `resumeOffset` após reconexão, com detecção de dessincronização.
- **`abortSenders` flag**: sinal atômico `atomic.Bool` para interromper waits e retries pendentes durante shutdown do dispatcher.
- **Script `scripts/check-missing-chunks.py`**: parser de session logs do server que identifica gaps de chunks faltantes para diagnóstico post-mortem.

### Alterado
- **ParallelJoin com JoinReason Flags**: o frame `PJIN` (v3.0.0+) agora inclui 1 byte de flags após o `StreamIndex`. Valores aceitos: `JoinReasonNone` (`0x00` — first-join ou reconexão por erro) e `JoinReasonRotation` (`0x01` — reconexão intencional por port rotation). O server utiliza esse flag para distinguir reconnects por falha de rotações de porta planejadas, evitando accountar erroneamente eventos de port rotation como falhas de stream.

### Motivação
> Na sessão de backup real, 5 chunks ficaram faltando porque o agent enviou `ControlIngestionDone` antes de o server confirmar via `ChunkSACK` que todos os bytes tinham sido recebidos. O `conn.Write()` local retornava sucesso, mas bytes ainda estavam em voo quando a conexão foi resetada. O Final ChunkSACK Drain garante que o shutdown só ocorre após confirmação real do server, eliminando essa classe de falhas.

---

## [v3.0.1] — 2026-03-01

Bugfix de SACK timeout false-positive no startup e correção da rotação de portas.

### Corrigido
- **SACK timeout false-positive no startup**: o timer `lastSACKAt` era inicializado em `ActivateStream()`, mas o producer só começa a gerar dados após todos os N streams serem ativados sequencialmente (~1.7s para 12 streams). Os primeiros streams excediam o timeout de 5s antes de qualquer dado ser enviado. O timer agora é inicializado no primeiro `writeFrame` bem-sucedido do sender.
- **`port_rotation.mode: "off"` ignorado**: o dispatcher lia `ChunksPerCycle` diretamente sem verificar o `Mode`. Método `EffectiveChunksPerCycle()` adicionado para retornar o ciclo apenas quando `mode: "per-n-chunks"`.

### Adicionado
- **Validação de `port_rotation.mode`**: valores aceitos são `"off"` e `"per-n-chunks"`. Valores desconhecidos retornam erro na validação do config.

---


## [v3.0.0] — 2026-03-01

Reescrita do subsistema de sessões paralelas com protocolo v5, Slot-based session management e remoção do GapTracker.

### Breaking Changes
- **Protocolo v5**: `ChunkHeader` agora é `[GlobalSeq uint32 4B] [Length uint32 4B] [SlotID uint8 1B]` (9 bytes). O campo `StreamIndex` foi substituído por `GlobalSeq` (sequência global do chunk) e adicionado `SlotID` (slot de origem). Clients v4 e anteriores **não são compatíveis** com servers v3.0.0.
- **Remoção do GapTracker**: `ControlNACK` e `ControlRetransmitResult` foram completamente removidos do protocolo. O `ChunkSACK` per-chunk acknowledgment oferece confiabilidade equivalente ou superior.
- **`gap_detection` deprecated**: a seção `gap_detection` no `server.yaml` é **ignorada** em runtime. Se presente, um `WARN` é emitido no startup.

### Adicionado
- **Slot-Based Session Management**: `ParallelSession` agora usa `[]*Slot` pré-alocados em vez de 12 `sync.Map`. Cada `Slot` possui estado tipado (`Idle`, `Receiving`, `Disconnected`, `Disabled`), métricas de chunks recebidos/perdidos/retransmitidos e controle de flow rotation por slot. Struct em `internal/server/slot.go`.
- **ControlSlotPark (CSLP)** e **ControlSlotResume (CSLR)**: novos frames de controle que permitem ao agent sinalizar scale-down (park) e scale-up (resume) de slots individuais.
- **Per-N-Chunk Port Rotation**: nova configuração `port_rotation` por backup entry. Quando `mode: "per-n-chunks"`, o agent desconecta e reconecta cada stream após enviar `chunks_per_cycle` chunks, rotacionando o source port TCP para evitar throttling por flow em middleboxes.
- **WebUI: Slot Status e Chunk Metrics**: a session detail view agora exibe o estado de cada slot (idle/receiving/disconnected/disabled) com métricas de chunks recebidos, perdidos e retransmitidos.
- **Prometheus-compatible metrics endpoint**: endpoint `/metrics` com métricas de bytes recebidos e sessões em formato Prometheus.
- **Auto-scaler freeze mode configurável**: modo de congelamento para desabilitar escalonamento automático durante operações críticas.

### Removido
- **GapTracker** (`gap_tracker.go`, `gap_tracker_test.go`): detectação proativa de gaps e chain NACK/RetransmitResult.
- **`ControlNACK`** e **`ControlRetransmitResult`**: frames de protocolo removidos.

### Motivação
> O GapTracker introduzia complexidade e overhead desnecessários. O ChunkSACK per-chunk acknowledgment, combinado com o Per-N-Chunk Port Rotation, oferece garantias de entrega equivalentes ou superiores com menor complexidade. A migração para `[]*Slot` elimina o overhead de type-assertion e sync.Map, melhora a legibilidade e permite métricas tipadas por slot.

---

## [v2.8.4] — 2026-02-26

Session logging por arquivo dedicado e diagnóstico aprimorado de assembly.

### Adicionado
- **Session Logger (`session_log_dir`)**: novo campo opcional em `logging` que cria um arquivo de log dedicado por sessão de backup paralelo. Fan-out: grava tanto no logger global quanto no arquivo `{session_log_dir}/{agent}/{sessionID}.log`. Arquivo removido automaticamente em backups OK, retido para post-mortem em falhas.
- **Log `chunk_received` (DEBUG)**: cada chunk recebido por stream é logado com `globalSeq`, `length`, `stream` e `totalBytes`. Capturado no arquivo de sessão sem poluir stdout.
- **Log `pre_finalize_state`**: dump do estado do assembler antes do Finalize (nextExpectedSeq, pendingChunks, pendingMemBytes, totalBytes, totalChunks, phase).
- **Log `missing_chunk_in_assembly`**: quando chunk está faltando no lazy assembly, agora loga `missingSeq`, `lazyMaxSeq` e `totalPending` para diagnóstico de gaps.
- **Resumo enriquecido do Finalize**: inclui `mode` e `pendingAtFinalize` no log de assembly finalizado.
- **`sessionID` no log de falha de drain do ChunkBuffer**: facilita correlação com session logs.

### Motivação
> Backups paralelos longos (~33GB, ~2h) falhavam com `missing chunk seq N in lazy assembly` sem contexto suficiente para diagnóstico. Os logs globais misturavam eventos de múltiplas sessões e streams, dificultando a análise post-mortem. O session logger resolve isso criando um arquivo isolado por sessão com rastreamento chunk a chunk.

---

## [v2.7.0] — 2026-02-23

Buffer de chunks em memória para suavizar I/O em discos lentos.

### Adicionado
- **Chunk Buffer global (`chunk_buffer`)**: nova configuração global do servidor que reserva um buffer em memória compartilhado entre todas as sessões de backup paralelo. Absorve chunks recebidos da rede e os drena assincronamente para o assembler, desacoplando I/O de rede do I/O de disco e reduzindo oscilações em HDs lentos (ex: USB, NAS mecânico).
  - `size`: tamanho máximo do buffer (ex: `"64mb"`, `"256mb"`). `0` desabilita (default).
  - `drain_ratio` (0.0–1.0, default 0.5): limiar de ocupação que aciona a drenagem.
    - `0.0` = write-through (drena imediatamente após cada chunk aceito em memória).
    - `0.5` = drena quando 50% da capacidade de bytes está em uso.
    - `1.0` = drena apenas quando o buffer está cheio.
  - O modo de escrita em disco respeita o `assembler_mode` do storage (`lazy`/`eager`).
  - **Fallback automático**: se um chunk exceder a capacidade disponível do buffer, é entregue diretamente ao assembler sem perda de dados.
  - **Flush garantido antes do `Finalize()`**: todos os chunks em memória são drenados antes da montagem final do arquivo.
  - **Backpressure**: se o canal estiver cheio por mais de 5s, o Push retorna erro e força reconexão do stream.
  - Quando `size: 0`, o caminho de código original é preservado sem qualquer overhead.
- **Cache de Storage Scan**: `StorageUsageSnapshot()` agora retorna dados de um cache atômico atualizado por ticker background. Parâmetro `storage_scan_interval` (default 1h, mínimo 30s) controla o intervalo de refresh. Elimina `syscall.Statfs` + `filepath.WalkDir` a cada request HTTP.

### Motivação
> Em HDs lentos (especialmente USB ou NAS mecânico), as goroutines de rede ficavam bloqueadas na escrita de cada chunk, causando throughput errático e quedas de velocidade. O buffer de memória age como um amortecedor: a rede escreve no buffer (rápido) e um worker dedicado drena para o disco uniformemente. O `drain_ratio` permite ajustar o trade-off entre latência (write-through a 0.0) e throughput (acúmulo a 0.5+).

---

## [v2.6.0] — 2026-02-21

Sharding de chunks configurável e fix crítico de retry.

### Adicionado
- **Sharding de chunks configurável (1 ou 2 níveis)**: campo `chunk_shard_levels` no config do storage. 2 níveis distribui chunks em 256×256 subpastas, ideal para storages com muitos backups paralelos.
- **Cache de `MkdirAll`**: evita chamadas repetidas para criar diretórios de sharding já existentes.

### Corrigido
- **Reset de retry counter**: o dispatcher agora reseta o contador de retries após reconexão bem-sucedida. Sem esse fix, após N falhas transientes o agent parava de tentar indefinidamente mesmo com conexão restabelecida.

### Motivação
> O sharding de 1 nível (256 subpastas) se mostrou insuficiente para volumes com dezenas de milhares de chunks simultâneos — o `readdir` ficava lento. O sharding de 2 níveis (65K subpastas) resolve, mas como é uma mudança de layout, foi feito configurável para retrocompatibilidade.

---

## [v2.5.3] — 2026-02-20

Correções de contagem e ETA + evolução do sharding + documentação.

### Adicionado
- **Emissão de eventos e logs ao rotacionar backups**: quando backups antigos são deletados, agora é emitido um evento observável na WebUI.
- **GitHub Wiki**: 9 páginas iniciais de documentação.

### Corrigido
- **`countBackups` recursivo**: antes só contava backups no primeiro nível de profundidade. Fix para percorrer todos os subdiretórios.
- **`countBackups` ignora `chunks_*`**: após o fix acima, o WalkDir passava a percorrer diretórios de sharding (256×256), causando lentidão severa. Fix para ignorar diretórios `chunks_*`.
- **Assembly ETA em modo lazy**: usava o timestamp de início da sessão em vez do início da fase de assembly, resultando em ETAs irreais.
- Call-site de `Rotate` no teste de integração corrigido para nova assinatura.

### Motivação
> Série de correções encadeadas: o `countBackups` que listava apenas `.tar.gz`/`.tar.zst` no nível raiz foi corrigido para ser recursivo. Porém isso revelou que o WalkDir traversava os diretórios `chunks_*` do sharding (potencialmente 65K pastas), gerando I/O desnecessário — corrigido com `filepath.SkipDir`. O ETA do assembler em modo lazy era calculado com base no início da sessão (hora da primeira conexão), e não do início real da montagem.

---

## [v2.5.2] — 2026-02-18

Sinal explícito de fim de ingestão e correção de deadlock do assembler.

### Adicionado
- **Frame `ControlIngestionDone` (CIDN)**: sinal explícito do agent informando que a transmissão de dados está completa. Remove a inferência implícita anterior (baseada em contagem de chunks), que era frágil.
- Testes de integração para control channel + CIDN.

### Corrigido
- **`ChunkAssembler.Stats()` lock-free**: `Stats()` usava lock que podia causar deadlock com o loop de assembly — travava a API `/api/v1/sessions/{id}` enquanto o assembler estivesse em flush. Refatorado para usar campos atômicos.
- CIDN carrega `sessionID` e falha explicitamente quando disconnected.

### Motivação
> Em produção, a WebUI ficava unresponsive durante o flush final do assembler. A causa era um deadlock entre o goroutine do assembler (que segurava o lock durante o write sequencial) e o handler da API (que tentava ler stats com o mesmo lock). A correção eliminou o lock e o CIDN garante consistência semântica do protocolo.

---

## [v2.5.1] — 2026-02-18

Bandwidth throttling e melhorias de histórico.

### Adicionado
- **Bandwidth throttling** por entry de backup: campo `bandwidth_limit` com token bucket (`golang.org/x/time/rate`). Suporta notações como `"100mb"`, `"512kb"`. Mínimo: 64kb/s.
- Integração do `ThrottledWriter` nos pipelines single-stream e paralelo.
- Persistência de histórico de sessões e suavização EWMA de throughput na WebUI.

### Motivação
> Em cenários WAN ou com storage lento, um backup sem limite de banda podia saturar o link e impactar outros serviços. O throttling por entry permite controlar isso sem afetar globalmente.

---

## [v2.5.0] / [v2.4.0] — 2026-02-17

> **Nota:** v2.4.0 e v2.5.0 apontam para o mesmo commit (release acidental duplicada).

Sharding de chunks + hash-based routing na WebUI.

### Adicionado
- **Directory sharding para chunks**: distribui chunks em 256 subpastas por hash, evitando diretórios com milhares de arquivos.
- **Hash-based routing na WebUI**: preserva a view ativa no refresh do browser (`#sessions/{id}`).

### Motivação
> Storages com muitos backups paralelos acumulavam milhares de chunks em um único diretório, degradando performance de `readdir`. O sharding distribui uniformemente.

---

## [v2.3.2] — 2026-02-17

Correções de UI responsiva.

### Corrigido
- **WebUI responsiva**: auto-scaler e info-grid adaptados para mobile.
- **Cores do gauge de eficiência**: corrigido para verde = bom, vermelho = ruim (estava invertido).
- **Status "scaling_up" incorreto**: quando já estava no máximo de streams, exibia "scaling up" em vez de "stable".

---

## [v2.3.1] — 2026-02-17

### Adicionado
- **Sessão visível durante finalização do assembler**: sessão permanece na lista `/api/v1/sessions` enquanto o assembler está na fase de escrita, com status `"finalizing"`.

### Motivação
> Antes, a sessão sumia da WebUI assim que o último stream desconectava, mesmo que o assembler ainda estivesse escrevendo no disco (o que podia levar minutos em backups grandes). Usuários achavam que o backup tinha falhado.

---

## [v2.3.0] — 2026-02-17

### Corrigido
- **Panic por WaitGroup reuse**: `sync.WaitGroup` era reutilizado durante teardown da sessão paralela, causando panic esporádico.

### Motivação
> Crash observado em produção quando duas goroutines tentavam fazer `Add()` e `Wait()` no mesmo WaitGroup após uma reconexão rápida.

---

## [v2.2.0] — 2026-02-17

Versão base rastreável. Inclui todo o histórico da v2.x.

### Highlights acumulados (pré-v2.2.0)
- Auto-scaler dual-mode (efficiency / adaptive) com stats pipeline.
- SPA de observabilidade completa: sessões, streams, eventos, config, agents, storages.
- Suporte a compressão Zstandard (zst) além de gzip.
- Assembly progress tracking com ETA em duas fases.
- Persistência JSONL de eventos e histórico de sessões.
- Tema dark/light, server stats no `/health`.
- Control channel bidirecional com handshake versionado.

### Corrigido
- Stream death quando `resumeOffset == rbHead` (todos os dados ACK'd).
