# WebUI — Painel de Observabilidade

O n-backup inclui uma **SPA (Single Page Application) embarcada** no binário do server (via `go:embed`), oferecendo um painel de observabilidade em tempo real para monitorar sessões de backup.

---

## Ativação

A WebUI é habilitada via configuração do server:

```yaml
webui:
  enabled: true
  listen_addr: ":8080"
  allowed_cidrs:
    - "10.0.0.0/8"
    - "192.168.0.0/16"
    - "127.0.0.0/8"
```

Acesse em `http://<server-ip>:8080` a partir de um IP autorizado nos `allowed_cidrs`.

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

### Session List

Cada sessão ativa é exibida como um card contendo:

- **Agent Name** e **Backup Name**
- **Storage** e **Compression** (gz/zst)
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
| **Streams** | Tabela detalhada de streams paralelos (index, status, bytes, uptime, reconexões) |
| **Progress** | Objetos enviados/total, ETA, barra de progresso |
| **SHA-256** | Hash calculado até o momento |
| **AutoScaler Stats** | Eficiência, modo, streams ativos/máx, throughput (apenas para backups paralelos) |

### Session History

As sessões completadas ficam acessíveis na aba de histórico, permitindo revisão de backups anteriores.

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

## API REST (Interna)

A WebUI consome uma API REST interna. Os endpoints disponíveis:

| Endpoint | Método | Descrição |
|----------|--------|-----------|
| `/api/sessions` | GET | Lista sessões ativas |
| `/api/sessions/:id` | GET | Detalhe de uma sessão |
| `/api/sessions/history` | GET | Sessões completadas |
| `/api/agents` | GET | Agents conectados via control channel |
| `/api/overview` | GET | Visão geral (server info, stats) |
| `/api/events` | GET | Eventos recentes |

> **Nota:** A API é interna e pode mudar entre versões. Não há garantia de estabilidade.

---

## Requisitos

- **Porta adicional**: A WebUI escuta em uma porta separada (default `:8080`), sem TLS por padrão
- **Acesso restrito**: Use `allowed_cidrs` para limitar acesso por rede
- **Zero dependências externas**: HTML, CSS e JS são embarcados no binário via `go:embed`
- **Sem banco de dados**: Os dados são mantidos em memória enquanto o server está ativo
