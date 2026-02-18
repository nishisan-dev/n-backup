# Configuração de Exemplo

Esta página apresenta os arquivos de configuração completos e comentados para o **agent** e o **server**.

---

## Agent (`agent.yaml`)

```yaml
# NBackup Agent — Exemplo de Configuração
# Copie para /etc/nbackup/agent.yaml e ajuste os valores.

agent:
  name: "web-server-01"

server:
  address: "backup.nishisan.dev:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/agent.pem
  client_key: /etc/nbackup/agent-key.pem

backups:
  - name: app
    storage: scripts             # Nome do storage no server
    schedule: "0 2 * * *"        # Cron: diário às 02h
    parallels: 0                 # 0 = single stream (padrão)
    # auto_scaler: efficiency    # efficiency (padrão) ou adaptive (usado com parallels > 0)
    # bandwidth_limit: "100mb"   # Limite de upload: 100 MB/s (opcional, mínimo 64kb)
    sources:
      - path: /app/scripts
    exclude:
      - "*.log"
      - "*.tmp"
      - ".git/**"

  - name: home
    storage: home-dirs           # Outro storage no server
    schedule: "0 */6 * * *"      # A cada 6 horas
    parallels: 4                 # 4 streams paralelos
    auto_scaler: adaptive        # Modo adaptive para link WAN variável
    bandwidth_limit: "50mb"      # Limite de 50 MB/s
    sources:
      - path: /home
    exclude:
      - ".cache/**"
      - "node_modules/**"
      - ".git/**"

retry:
  max_attempts: 5                # Máximo de tentativas
  initial_delay: 1s              # Delay da 1ª retentativa
  max_delay: 5m                  # Delay máximo (cap do backoff)

resume:
  buffer_size: 256mb             # Tamanho do ring buffer (kb, mb, gb)
  chunk_size: 1mb                # Tamanho de cada chunk paralelo (64kb-16mb)

logging:
  level: info                    # debug | info | warn | error
  format: json                   # json | text

daemon:
  control_channel:
    enabled: true                # Ativar canal de controle
    keepalive_interval: 30s      # Intervalo entre PINGs (≥ 1s)
    reconnect_delay: 5s          # Delay inicial de reconexão
    max_reconnect_delay: 5m      # Delay máximo do backoff
```

### Campos Importantes

| Campo | Obrigatório | Descrição |
|-------|:-----------:|-----------|
| `agent.name` | ✅ | Identificador único. **Deve casar com o CN do certificado TLS.** |
| `server.address` | ✅ | Endereço `host:porta` do server |
| `tls.*` | ✅ | Caminhos para CA, certificado e chave do agent |
| `backups[].name` | ✅ | Nome lógico do backup entry |
| `backups[].storage` | ✅ | Nome do storage **existente** no server |
| `backups[].schedule` | ✅ | Cron expression (padrão Unix) |
| `backups[].sources` | ✅ | Lista de diretórios a incluir no backup |
| `backups[].exclude` | ❌ | Padrões glob de exclusão |
| `backups[].parallels` | ❌ | `0` = single stream (padrão), `1-255` = streams paralelos |
| `backups[].auto_scaler` | ❌ | `efficiency` (padrão) ou `adaptive` |
| `backups[].bandwidth_limit` | ❌ | Limite de upload em Bytes/s (ex: `50mb`, `1gb`, `256kb`). Mínimo: `64kb`. |
| `retry.*` | ❌ | Configuração de retry (defaults sensatos se omitido) |
| `resume.buffer_size` | ❌ | Default: `256mb`. Aceita: `kb`, `mb`, `gb` |
| `resume.chunk_size` | ❌ | Default: `1mb`. Range: `64kb` a `16mb` |
| `daemon.control_channel.*` | ❌ | Canal de controle (default: habilitado) |

---

## Server (`server.yaml`)

```yaml
# NBackup Server — Exemplo de Configuração
# Copie para /etc/nbackup/server.yaml e ajuste os valores.

server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  scripts:                         # Nome lógico do storage
    base_dir: /var/backups/scripts # Diretório base no filesystem
    max_backups: 5                 # Manter os N mais recentes por agent
    compression_mode: gz           # gz (padrão) ou zst
    assembler_mode: eager          # eager (padrão) ou lazy
    assembler_pending_mem_limit: 8mb  # Limite de memória para chunks OOO (usado em eager)

  home-dirs:
    base_dir: /var/backups/home
    max_backups: 10
    compression_mode: zst          # Zstandard
    assembler_mode: lazy           # staging completo antes de montar
    assembler_pending_mem_limit: 8mb  # ignorado em modo lazy

logging:
  level: info                      # debug | info | warn | error
  format: json                     # json | text

webui:
  enabled: true                    # Ativar WebUI de observabilidade
  listen_addr: ":8080"             # Endereço de escuta da WebUI
  allowed_cidrs:                   # CIDRs permitidos (restrição de acesso)
    - "10.0.0.0/8"
    - "192.168.0.0/16"
    - "127.0.0.0/8"
```

### Campos Importantes

| Campo | Obrigatório | Descrição |
|-------|:-----------:|-----------|
| `server.listen` | ✅ | Endereço de escuta `bind:porta` |
| `tls.*` | ✅ | Caminhos para CA, certificado e chave do server |
| `storages.<nome>.base_dir` | ✅ | Diretório base do storage |
| `storages.<nome>.max_backups` | ❌ | Quantos backups manter por agent (rotação) |
| `storages.<nome>.compression_mode` | ❌ | `gz` (padrão) ou `zst` (Zstandard) |
| `storages.<nome>.assembler_mode` | ❌ | `eager` (padrão) ou `lazy` |
| `storages.<nome>.assembler_pending_mem_limit` | ❌ | Default: `8mb`. Limite de memória para chunks out-of-order (ignorado em lazy). |
| `webui.enabled` | ❌ | `true` ativa a WebUI (default: `false`) |
| `webui.listen_addr` | ❌ | Porta da WebUI (default: `:8080`) |
| `webui.allowed_cidrs` | ❌ | Restrição de acesso por IP/CIDR |

---

## Observações

1. **Correspondência CN ↔ agent.name**: O server valida que o CN do certificado TLS do agent corresponde ao campo `agent.name`. Se não corresponderem, a conexão é rejeitada.
2. **Storage deve existir**: O `storage` referenciado nos backups do agent deve estar configurado no server. Caso contrário, o server responde `STORAGE_NOT_FOUND`.
3. **Criar diretórios**: Os `base_dir` dos storages devem existir e ter permissão de escrita.
4. **Resume**: O `buffer_size` define quanto dado o agent mantém em memória. Quanto maior, mais tolerante a quedas de conexão longas.
5. **Parallel + AutoScaler**: O auto-scaler só é relevante quando `parallels > 0`. Caso contrário, é ignorado.
6. **Bandwidth Throttling**: O `bandwidth_limit` aplica-se ao throughput agregado do backup entry. Para single-stream, limita a conexão única. Para parallel-stream, limita a soma de todos os streams. Mínimo aceito: `64kb`.
