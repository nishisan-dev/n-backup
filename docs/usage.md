# Guia de Uso

## Comandos

### nbackup-agent

| Modo | Comando | Descrição |
|------|---------|-----------|
| Daemon | `nbackup-agent --config agent.yaml` | Executa como daemon, backups automáticos via cron |
| Once | `nbackup-agent --config agent.yaml --once` | Executa um backup e encerra |
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

O fluxo completo de um backup é:

```
1. Agent conecta ao Server via TLS 1.3 (mTLS)
2. Handshake de protocolo (nome do agent, versão)
3. Server responde ACK (GO / BUSY / REJECT)
4. Agent faz streaming: scan → tar → gzip → rede
5. Agent envia trailer com SHA-256 e tamanho
6. Server valida checksum, faz commit atômico (.tmp → rename)
7. Server executa rotação (remove backups excedentes)
8. Server envia Final ACK (OK / CHECKSUM_MISMATCH / WRITE_ERROR)
```

### Fluxo de Dados (zero-copy)

```
Disco origem → tar.Writer → gzip.Writer → TLS conn → Server (io.Copy → disco destino)
```

O SHA-256 é calculado **inline** (sem releitura), sem arquivos temporários na origem.

---

## Configuração de Sources e Excludes

### Sources

Lista de diretórios a serem incluídos no backup:

```yaml
backup:
  sources:
    - path: /app/scripts
    - path: /home
    - path: /etc
```

Cada source gera entradas no tar com **caminhos relativos** baseados no próprio diretório.

### Excludes (Glob Patterns)

Padrões glob para excluir arquivos e diretórios:

```yaml
backup:
  exclude:
    - "*.log"              # Todos os arquivos .log
    - ".git/**"            # Diretório .git recursivo
    - "node_modules/**"    # Dependências Node.js
    - "*/tmp/sess*"        # Sessões temporárias
    - "*/access-logs/"     # Diretório access-logs
```

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

## Rotação Automática (Server)

O server mantém no máximo `max_backups` por agent. Os mais antigos são removidos automaticamente após cada backup bem-sucedido.

```yaml
storage:
  max_backups: 5
```

Exemplo com `max_backups: 3`:

```diff
  /var/backups/nbackup/web-server-01/
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
| `server rejected: status=2` | Backup já em andamento para este agent | Aguardar conclusão do backup anterior |
| `server rejected: status=1` | Disco cheio no server | Liberar espaço ou ajustar `max_backups` |
| `checksum mismatch` | Corrupção de dados na rede | O backup é descartado; será retentado |
| `all N attempts failed` | Server persistentemente indisponível | Verificar conectividade e logs do server |
