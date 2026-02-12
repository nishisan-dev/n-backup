# n-backup

Sistema de backup high-performance client-server escrito em Go. Realiza streaming de dados da origem para o destino sem criação de arquivos temporários, utilizando comunicação TCP pura com mTLS.

## Pilares

- **Streaming Nativo** — Pipeline `Disk → Tar → Gzip → Network` via `io.Pipe`. Zero arquivos temporários na origem.
- **Zero-Footprint** — Otimizado para baixo consumo de CPU e RAM.
- **Segurança mTLS** — Autenticação mútua obrigatória entre Agent e Server via TLS 1.3.
- **Resiliência** — Retry com Exponential Backoff, escrita atômica com validação SHA-256.

## Arquitetura

![Arquitetura StreamGuard](https://uml.nishisan.dev/proxy?src=https://raw.githubusercontent.com/nishisan-dev/n-backup/main/docs/diagrams/architecture.puml)

## Componentes

| Componente | Descrição |
|---|---|
| **nbackup-agent** | Daemon que executa backups periodicamente (cron expression). Lê arquivos, compacta e envia via TCP+mTLS. |
| **nbackup-server** | Recebe streams de backup, valida integridade (SHA-256), grava em disco e faz rotação automática. |

## Formato de Saída

Arquivo `.tar.gz` padrão Linux. Para restaurar:

```bash
tar xzf backup.tar.gz -C /restore/path
```

## Uso

```bash
# Agent (daemon mode)
nbackup-agent --config /etc/nbackup/agent.yaml

# Agent (execução única)
nbackup-agent --config agent.yaml --once

# Health check
nbackup-agent health backup.nishisan.dev:9847

# Server
nbackup-server --config /etc/nbackup/server.yaml
```

## Configuração

Exemplos em [`configs/`](configs/).

## Documentação

- [Especificação Técnica](docs/specification.md)
- Diagramas: [`docs/diagrams/`](docs/diagrams/)

## Stack

| Recurso | Tecnologia |
|---|---|
| Linguagem | Go |
| Transporte | TCP + TLS 1.3 (mTLS) |
| Compactação | gzip (stdlib) |
| Empacotamento | tar (stdlib) |
| Configuração | YAML |
| Logging | slog (JSON) |
| Distribuição | Binário estático |

## Licença

Proprietário — Uso interno.
