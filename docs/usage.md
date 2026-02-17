# Guia de Uso

## Comandos

### nbackup-agent

| Modo | Comando | Descrição |
|------|---------|-----------|
| Daemon | `nbackup-agent --config agent.yaml` | Executa como daemon, backups automáticos via cron |
| Once | `nbackup-agent --config agent.yaml --once` | Executa um backup e encerra |
| Once + Progress | `nbackup-agent --config agent.yaml --once --progress` | Backup manual com barra de progresso |
| Health | `nbackup-agent health <addr>` | Verifica status do server |

### nbackup-server

| Modo | Comando | Descrição |
|------|---------|-----------|
| Listen | `nbackup-server --config server.yaml` | Aceita conexões de backup |

---

## Daemon Mode (padrão)

O agent roda como processo permanente e dispara backups conforme a cron expression configurada:

```bash
nbackup-agent --config /etc/nbackup/agent.yaml
```

O scheduler utiliza o mesmo formato cron do Unix:

```yaml
daemon:
  schedule: "0 2 * * *"    # Diário às 02h
```

Exemplos de expressões:

| Expressão | Frequência |
|-----------|-----------|
| `0 2 * * *` | Diário às 02:00 |
| `0 */6 * * *` | A cada 6 horas |
| `0 3 * * 0` | Semanal, domingos às 03:00 |
| `0 0 1 * *` | Mensal, dia 1 à meia-noite |

> [!NOTE]
> Se um backup anterior ainda estiver em execução quando o scheduler disparar, a execução é ignorada para evitar sobrecarga.

---

## Execução Única

Para executar um backup manualmente sem iniciar o daemon:

```bash
nbackup-agent --config /etc/nbackup/agent.yaml --once
```

Útil para:
- Testes iniciais de conectividade
- Backups ad-hoc antes de manutenção
- Execução via crontab externo

### Progress Bar (`--progress`)

Para acompanhar o progresso visualmente:

```bash
nbackup-agent --config /etc/nbackup/agent.yaml --once --progress
```

Output no terminal:

```
[app] ████████████░░░░░░░░░░░░░░░░  42.3 MB  │  12.8 MB/s  │  1,247 objs (831/s)  │  0:03  │  ETA 0:07
```

| Campo | Descrição |
|-------|-----------|
| `[nome]` | Nome do backup entry em execução |
| Barra | Progresso proporcional (estimativa baseada em compressão ~50%) |
| Bytes | Total compactado enviado ao server |
| MB/s | Velocidade de transferência |
| objs (n/s) | Objetos processados e taxa por segundo |
| Elapsed | Tempo decorrido desde o início |
| ETA | Tempo estimado restante |
| retries | Mostrado somente se houve tentativas de reconexão |

> [!NOTE]
> O ETA é calculado com base na velocidade média observada. A estimativa de total considera ~50% de compressão gzip sobre o tamanho raw dos arquivos.

> [!TIP]
> A flag `--progress` só funciona com `--once`. No modo daemon os logs são suficientes.

---

## Health Check

Verifica se o server está acessível e operante:

```bash
nbackup-agent health backup.nishisan.dev:9847

# Com config customizado (necessário para TLS)
nbackup-agent health backup.nishisan.dev:9847 --config /etc/nbackup/agent.yaml
```

Respostas possíveis:

| Status | Significado |
|--------|-------------|
| `READY` | Server operacional |
| `BUSY` | Server aceitando mas sob carga |
| `LOW DISK` | Espaço em disco baixo |
| `MAINTENANCE` | Server em manutenção |

---

## Backup: O que Acontece

Cada backup entry na configuração é executado sequencialmente. Para cada entry:

```
1. Agent conecta ao Server via TLS 1.3 (mTLS)
2. Handshake: agent name + storage name + versão do protocolo
3. Server busca o storage nomeado no mapa de storages
4. Server responde ACK (GO / BUSY / REJECT / STORAGE_NOT_FOUND)
5. Agent faz streaming: scan → tar → gzip → rede
6. Agent envia trailer com SHA-256 e tamanho
7. Server valida checksum, faz commit atômico (.tmp → rename)
8. Server executa rotação no storage correspondente
9. Server envia Final ACK (OK / CHECKSUM_MISMATCH / WRITE_ERROR)
```

### Modelo N:N

Um agent pode ter múltiplos backup entries, cada um direcionado a um storage diferente no server. O lock é por `agent:storage`, permitindo backups simultâneos de storages diferentes.

### Fluxo de Dados (zero-copy)

```
Disco origem → tar.Writer → gzip.Writer → TLS conn → Server (io.Copy → disco destino)
```

O SHA-256 é calculado **inline** (sem releitura), sem arquivos temporários na origem.

---

## Configuração de Backups (Agent)

Cada backup entry define quais diretórios incluir e para qual storage do server enviar:

```yaml
backups:
  - name: app               # Nome lógico do backup
    storage: scripts         # Storage nomeado no server
    parallels: 0             # 0 = single stream (padrão)
    sources:
      - path: /app/scripts
    exclude:
      - "*.log"

  - name: home
    storage: home-dirs
    parallels: 4             # 4 streams paralelos
    sources:
      - path: /home
    exclude:
      - ".git/**"
      - "node_modules/**"
```

Cada source gera entradas no tar com **caminhos relativos** baseados no próprio diretório.

---

## Retry com Exponential Backoff

Se o backup falhar (erro de rede, server indisponível), o agent retenta automaticamente:

```yaml
retry:
  max_attempts: 5       # Máximo de tentativas
  initial_delay: 1s     # Delay da primeira retentativa
  max_delay: 5m         # Delay máximo (cap do backoff)
```

O delay cresce exponencialmente:

| Tentativa | Delay |
|-----------|-------|
| 1ª | 0s (imediata) |
| 2ª | 1s |
| 3ª | 2s |
| 4ª | 4s |
| 5ª | 8s (ou max_delay) |

---

## Resume Automático

Para backups grandes (>1GB), o agent mantém um **ring buffer** em memória. Se a conexão cair mid-stream, o agent reconecta e retoma de onde parou sem reenviar tudo.

```yaml
resume:
  buffer_size: 256mb    # Tamanho do ring buffer (kb, mb, gb, default: 256mb)
  chunk_size: 1mb       # Tamanho de cada chunk paralelo (64kb-16mb, default: 1mb)
```

### Como Funciona

```
1. Agent envia dados via ring buffer (backpressure se cheio)
2. Server confirma recebimento a cada 64MB (SACK)
3. Agent libera espaço no buffer após cada SACK
4. Se conexão cair: agent reconecta e envia RESUME + sessionID
5. Server responde com último offset gravado em disco
6. Agent retoma envio do offset (se ainda no buffer)
```

### Parâmetros

| Parâmetro | Default | Descrição |
|----------|---------|----------|
| `resume.buffer_size` | `256mb` | Tamanho do ring buffer |
| `resume.chunk_size` | `1mb` | Tamanho de cada chunk paralelo (64kb-16mb) |
| SACK interval (fixo) | 1MB | Server confirma a cada 1MB |
| Max resume attempts (fixo) | 5 | Tentativas antes de reiniciar |
| Session TTL (fixo) | 1h | Tempo máximo para reconectar |

> [!TIP]
> Para backups de 700GB+, considere aumentar o buffer para `1gb` para tolerar interrupções mais longas.

> [!IMPORTANT]
> Se o offset não estiver mais no ring buffer (avançou além da capacidade), o backup reinicia do zero.

---

## Parallel Streaming

Para aumentar o throughput de backups grandes, o agent pode usar **múltiplos streams TLS paralelos**:

```yaml
backups:
  - name: "data"
    storage: "main"
    parallels: 4             # 0 = single stream, 1-8 = máximo de streams
```

### Como Funciona

```
1. Agent envia Handshake normal, recebe ACK GO + SessionID
2. Agent envia ParallelInit (maxStreams, chunkSize) na conexão primária
3. Agent abre conexões TLS adicionais com ParallelJoin (sessionID, streamIndex)
4. Dados são distribuídos round-robin entre streams pelo Dispatcher
5. AutoScaler ajusta dinamicamente o número de streams ativos
6. Server reassembla chunks na ordem correta e faz commit atômico
```

### Parâmetros

| Parâmetro | Default | Descrição |
|----------|---------|----------|
| `parallels` | `0` | Número máximo de streams (0=desabilita) |
| `resume.chunk_size` | `1mb` | Tamanho de cada chunk distribuído (64kb-16mb) |
| Hysteresis window (fixo) | 3 | Janelas consecutivas para escalar |

> [!TIP]
> Use `parallels: 2-4` para links com laten̂ncia alta (WAN). Para LAN, `parallels: 0` costuma ser suficiente.

> [!NOTE]
> O AutoScaler adiciona streams gradualmente com base na eficiência observada (razão producer/drain), evitando overhead desnecessário.

---

## Control Channel

O agent mantém uma conexão TLS persistente com o server para keep-alive, medição de RTT e orquestração:

```yaml
daemon:
  control_channel:
    enabled: true                # Ativar canal de controle (default: true)
    keepalive_interval: 30s      # Intervalo entre PINGs (≥ 1s)
    reconnect_delay: 5s          # Delay inicial de reconexão
    max_reconnect_delay: 5m      # Delay máximo (exponential backoff)
```

### Funções

| Função | Descrição |
|--------|----------|
| **Keep-alive** | PINGs periódicos detectam desconexão proativamente |
| **RTT EWMA** | Medição contínua de latência via EWMA (α = 0.25) |
| **Status do Server** | Carga de CPU e espaço livre em disco reportados no Pong |
| **Graceful Flow Rotation** | Server solicita drenagem de stream via `ControlRotate` — zero data loss |

> [!NOTE]
> O control channel opera independentemente dos streams de dados. Se desabilitado (`enabled: false`), o agent funciona normalmente mas sem keep-alive e sem flow rotation graceful.

> [!TIP]
> Em links WAN com latência alta, aumente o `keepalive_interval` para 60s ou mais para reduzir overhead.

---

## Rotação Automática (Server)

Cada storage nomeado mantém no máximo `max_backups` por agent. Os mais antigos são removidos automaticamente após cada backup bem-sucedido.

```yaml
storages:
  scripts:
    base_dir: /var/backups/scripts
    max_backups: 5
    assembler_mode: eager
    assembler_pending_mem_limit: 8mb
  home-dirs:
    base_dir: /var/backups/home
    max_backups: 10
    assembler_mode: lazy
    assembler_pending_mem_limit: 8mb
```

Defaults por storage:
- `assembler_mode`: `eager`
- `assembler_pending_mem_limit`: `8mb` (8 * 1024 * 1024 bytes)

Comportamento dos modos:
- `eager`: monta incrementalmente durante a transferência. Chunks fora de ordem ficam em memória até `assembler_pending_mem_limit`; ao exceder, fazem spill para disco.
- `lazy`: grava os chunks em staging e monta somente no final da sessão. Nesse modo, `assembler_pending_mem_limit` não é usado.

Exemplo com `max_backups: 3` no storage `scripts`:

```diff
  /var/backups/scripts/web-server-01/
- 2026-02-08T02-00-00.tar.gz   ← removido
- 2026-02-09T02-00-00.tar.gz   ← removido
  2026-02-10T02-00-00.tar.gz
  2026-02-11T02-00-00.tar.gz
+ 2026-02-12T02-00-00.tar.gz   ← novo
```

---

## Restauração

O n-backup v1 não inclui restore automatizado. Os backups são arquivos `.tar.gz` padrão:

```bash
# Listar conteúdo
tar tzf /var/backups/nbackup/web-server-01/2026-02-12T02-00-00.tar.gz

# Restaurar completo
tar xzf 2026-02-12T02-00-00.tar.gz -C /restore/path

# Restaurar arquivo específico
tar xzf 2026-02-12T02-00-00.tar.gz -C /restore/path home/user/file.txt
```

---

## Logging

Ambos os componentes usam `slog` com saída estruturada:

```yaml
logging:
  level: info    # debug | info | warn | error
  format: json   # json | text
```

Exemplo de log JSON do agent:

```json
{"time":"2026-02-12T02:00:01Z","level":"INFO","msg":"starting backup session","server":"backup.nishisan.dev:9847"}
{"time":"2026-02-12T02:00:01Z","level":"INFO","msg":"handshake successful, starting data transfer"}
{"time":"2026-02-12T02:00:15Z","level":"INFO","msg":"data transfer complete","bytes":52428800,"checksum":"9f6c7091..."}
{"time":"2026-02-12T02:00:16Z","level":"INFO","msg":"backup completed successfully","bytes":52428800}
```

---

## Troubleshooting

| Sintoma | Causa Provável | Solução |
|---------|---------------|---------|
| `connection refused` | Server não está rodando ou porta errada | Verificar `systemctl status nbackup-server` |
| `tls: bad certificate` | Certificado do agent não assinado pela CA | Regenerar cert com a mesma CA |
| `server rejected: status=2` | Backup já em andamento (agent:storage) | Aguardar conclusão do backup anterior |
| `server rejected: status=1` | Disco cheio no server | Liberar espaço ou ajustar `max_backups` |
| `storage not found` | Nome do storage não existe no server | Verificar `storages:` no server.yaml |
| `checksum mismatch` | Corrupção de dados na rede | O backup é descartado; será retentado |
| `all N attempts failed` | Server persistentemente indisponível | Verificar conectividade e logs do server |
| `offset no longer in buffer` | Ring buffer overflow durante resume | Aumentar `resume.buffer_size` ou melhorar rede |
| `session not found for resume` | Sessão expirou no server (>1h) | O backup reiniciará automaticamente |
| `max resume attempts reached` | 5 tentativas de resume falharam | Verificar estabilidade da rede |
| `control channel disconnected` | Server caiu ou timeout de keepalive | Canal reconectará automaticamente com backoff |
| `keepalive_interval must be >= 1s` | Valor inválido na config | Ajustar para ≥ 1s |

---

## WebUI de Observabilidade (v2.0.0+)

O nbackup-server inclui uma interface web embarcada para monitoramento operacional em tempo real.

### Configuração

```yaml
observability:
  enabled: true
  listen_addr: ":8080"
  allowed_cidrs:
    - "10.0.0.0/8"
    - "127.0.0.1/32"
  events_max_lines: 10000
  session_history_size: 200
```

| Parâmetro | Default | Descrição |
|----------|---------|-----------|
| `enabled` | `false` | Habilita o listener HTTP da WebUI |
| `listen_addr` | `:8080` | Endereço de bind (host:porta) |
| `allowed_cidrs` | — | CIDRs autorizados a acessar |
| `events_max_lines` | `10000` | Máximo de linhas no JSONL de eventos antes de rotação |
| `session_history_size` | `200` | Sessões finalizadas mantidas no ring buffer |

### Acesso

Abra `http://<server-ip>:8080` em qualquer navegador. A interface não requer autenticação (protegida por ACL de rede).

### Views

| View | Conteúdo |
|------|----------|
| **Overview** | Status do server, métricas gerais, agentes conectados com gauges de CPU/RAM/Disco, storages com uso de disco |
| **Sessions** | Sessões ativas com sparklines de throughput, detalhes de streams (uptime, reconnects), assembler progress, e **histórico de sessões finalizadas** com badges de resultado |
| **Events** | Eventos recentes (ring buffer + persistência JSONL) |
| **Config** | Configuração efetiva do server (read-only) |

### Badges de Resultado (Session History)

| Badge | Significado |
|-------|-------------|
| ✓ ok | Backup finalizado com sucesso |
| ✗ checksum | Checksum mismatch na validação |
| ⚠ write error | Erro de escrita no storage |
| ⏱ timeout | Sessão expirou por timeout |
| ✗ error | Erro genérico |

> [!TIP]
> A WebUI atualiza automaticamente a cada 2 segundos via polling. Não é necessário refresh manual.

---

## Considerações por Tipo de Disco/Storage

O modo do assembler e o limite de memória ideal variam conforme o tipo de disco no server.

### 1. USB/HDD lento (maior variação de latência)

Preferir `assembler_mode: lazy` para reduzir pressão de I/O durante a transferência.

```yaml
storages:
  usb-archive:
    base_dir: /mnt/usb/backups
    max_backups: 7
    assembler_mode: lazy
    assembler_pending_mem_limit: 8mb  # ignorado em lazy
```

### 2. SSD SATA (equilíbrio)

Usar `eager` com limite moderado para absorver out-of-order sem spill frequente.

```yaml
storages:
  sata-fast:
    base_dir: /var/backups/sata
    max_backups: 10
    assembler_mode: eager
    assembler_pending_mem_limit: 16mb
```

### 3. NVMe/Storage rápido

Usar `eager` com limite maior para minimizar spill em disco.

```yaml
storages:
  nvme-hot:
    base_dir: /var/backups/nvme
    max_backups: 20
    assembler_mode: eager
    assembler_pending_mem_limit: 64mb
```

Notas:
- Defaults por storage: `assembler_mode: eager` e `assembler_pending_mem_limit: 8mb` (`8 * 1024 * 1024` bytes).
- Em `lazy`, `assembler_pending_mem_limit` não é utilizado.
- Se houver pressão de memória no host, reduza `assembler_pending_mem_limit` ou mude para `lazy`.
