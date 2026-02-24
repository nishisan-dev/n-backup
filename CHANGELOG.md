# Changelog

Todas as mudanças notáveis do projeto são documentadas neste arquivo.

O formato é baseado em [Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/)
e o versionamento segue [Semantic Versioning](https://semver.org/lang/pt-BR/).

---

## [Unreleased]

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
