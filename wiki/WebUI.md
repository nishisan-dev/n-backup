# WebUI — Painel de Observabilidade

O n-backup inclui uma **SPA (Single Page Application) embarcada** no binário do server (via `go:embed`), oferecendo um painel de observabilidade em tempo real para monitorar sessões de backup.

![Overview da WebUI](https://raw.githubusercontent.com/nishisan-dev/n-backup/main/wiki/images/webui_overview.png)

---

## Ativação

A WebUI é habilitada via configuração do server:

```yaml
web_ui:
  enabled: true
  listen: "127.0.0.1:9848"         # Bind default: loopback (segurança)
  read_timeout: 5s
  write_timeout: 15s
  idle_timeout: 60s

  # Persistência de dados entre reinicios do server
  events_file: /var/lib/nbackup/events.jsonl
  events_max_lines: 10000
  session_history_file: /var/lib/nbackup/session-history.jsonl
  session_history_max_lines: 5000
  active_sessions_file: /var/lib/nbackup/active-sessions.jsonl
  active_sessions_max_lines: 20000
  active_snapshot_interval: 5m

  allow_origins:                    # ACL obrigatória (IPs ou CIDRs)
    - "127.0.0.1/32"
    - "10.0.0.0/8"
    - "192.168.0.0/16"
```

Acesse em `http://<server-ip>:9848` a partir de um IP autorizado nos `allow_origins`.

> **Nota:** Para expor a WebUI em outra interface, altere `listen`. Use um reverse proxy (nginx/Caddy) com TLS para exposição externa.

---

## Funcionalidades

### Overview

A página principal exibe:

| Seção | Descrição |
|-------|-----------|
| **Server Info** | Versão do server, uptime, estatísticas globais |
| **Connected Agents** | Tabela de agents conectados via control channel, com IP, versão, RTT, uptime e métricas do sistema (CPU, memória, disco via gauges visuais) |
| **Active Sessions** | Sessões de backup em andamento |
| **Recent Events** | Timeline de eventos do server (início/fim de sessão, reconexões, rotações) |

> A imagem no topo desta página mostra a view Overview com dados reais de um ambiente de produção.

### Session List

![Sessões da WebUI](https://raw.githubusercontent.com/nishisan-dev/n-backup/main/wiki/images/webui_sessions.png)

Cada sessão ativa é exibida como um card contendo:

- **Agent Name** e **Backup Name**
- **Storage** e **Compression** (gzip/zst)
- **Protocol Version**
- **Bytes transferidos** e **velocidade** (MB/s)
- **Progresso** (barra de progresso, quando disponível)
- **Mini sparkline** de throughput
- **Status** (streaming, assembling, completed, failed)

### Session Detail

Ao clicar em uma sessão, visualize:

| Métrica | Descrição |
|---------|-----------|
| **Transfer Speed** | Sparkline interativo com histórico de velocidade |
| **Disk I/O** | Sparkline de I/O de escrita no server |
| **Buffer Usage** | Sparkline de uso do chunk buffer (quando habilitado no server) |
| **Streams** | Tabela detalhada de streams paralelos (index, status, bytes, uptime, reconexões) |
| **Slot Status** | Estado de cada slot: `Idle`, `Receiving`, `Disconnected`, `Disabled` (v3.0.0+) |
| **Chunk Metrics** | Métricas por slot: chunks recebidos, perdidos e retransmitidos (v3.0.0+) |
| **Progress** | Objetos enviados/total, ETA, barra de progresso |
| **SHA-256** | Hash calculado até o momento |
| **AutoScaler Stats** | Eficiência, modo, streams ativos/máx, throughput (apenas para backups paralelos) |

### Session History

As sessões completadas ficam acessíveis na aba de histórico, **persistidas em disco** via `session_history_file` — disponíveis mesmo após reinicios do server.

### Eventos

![Eventos da WebUI](https://raw.githubusercontent.com/nishisan-dev/n-backup/main/wiki/images/webui_events.png)

Timeline de eventos do server em tempo real, com filtros de quantidade e exportação em JSON.

### Configuração Efetiva

![Config da WebUI](https://raw.githubusercontent.com/nishisan-dev/n-backup/main/wiki/images/webui_config.png)

Visualização da configuração efetiva do server em formato JSON.

---

## Gauges e Sparklines

### Agent Gauges

Para cada agent conectado, a WebUI exibe progress bars visuais para:

| Métrica | Visualização |
|---------|-------------|
| CPU | Gauge colorido (verde → amarelo → vermelho) |
| Memória | Gauge com uso percentual |
| Disco | Gauge com espaço utilizado |

### AutoScaler Gauges (v2.1.2+)

Para sessões paralelas com auto-scaler ativo:

| Métrica | Visualização |
|---------|-------------|
| Efficiency | Gauge (verde = bom, vermelho = gargalo) |
| Streams | Barra com ativos/máximo |
| State | Badge (Stable, Scaling Up, Scale Down, Probing) |

### Transfer Sparklines

Gráficos de linha em tempo real mostrando:
- **Network throughput** (MB/s) — taxa de transferência
- **Disk I/O** (MB/s) — velocidade de escrita no server

---

## Persistência de Dados

Os dados da WebUI são **mantidos em memória** durante a execução e **opcionalmente persistidos em disco**:

| Campo | Arquivo JSONL | Descrição |
|-------|--------------|-----------|
| `events_file` | `events.jsonl` | Timeline de eventos (início/fim de sessão, rotações, reconexões) |
| `session_history_file` | `session-history.jsonl` | Histórico de backups completados |
| `active_sessions_file` | `active-sessions.jsonl` | Snapshots periódicos de sessões ativas |

O `active_snapshot_interval` (default: `5m`) controla frequência dos snapshots de sessões ativas — útil para diagnóstico após crash.

> **Dica:** Crie os diretórios dos arquivos antes de iniciar o server: `mkdir -p /var/lib/nbackup`

---

## API REST (Interna)

A WebUI consome uma API REST interna. Os endpoints disponíveis:

| Endpoint | Método | Descrição |
|----------|--------|-----------|
| `/api/v1/sessions` | GET | Lista sessões ativas |
| `/api/v1/sessions/:id` | GET | Detalhe de uma sessão (inclui slot status e chunk metrics) |
| `/api/v1/sessions/history` | GET | Sessões completadas |
| `/api/v1/sessions/active-history` | GET | Snapshots periódicos de sessões ativas |
| `/api/v1/agents` | GET | Agents conectados via control channel |
| `/api/v1/health` | GET | Status do server |
| `/api/v1/metrics` | GET | Bytes recebidos, sessões |
| `/api/v1/storages` | GET | Storages com uso de disco |
| `/api/v1/events` | GET | Eventos recentes |
| `/api/v1/config/effective` | GET | Configuração efetiva do server |
| `/api/v1/sync/status` | GET | Status e progresso do sync retroativo de Object Storage |
| `/metrics` | GET | Métricas Prometheus-compatíveis (v3.0.0+) |

> **Nota:** A API é interna e pode mudar entre versões. Não há garantia de estabilidade.

---

## Requisitos

- **Porta adicional**: A WebUI escuta em uma porta separada (default `127.0.0.1:9848`), sem TLS por padrão
- **ACL obrigatória**: `allow_origins` é obrigatório quando `enabled: true`. Aceita IPs puros (expandidos para `/32`) e CIDRs
- **Zero dependências externas**: HTML, CSS e JS são embarcados no binário via `go:embed`
- **Persistência opcional**: Configure os arquivos `*_file` para que eventos e histórico sobrevivam a reinicios
