# Cenários de Configuração

Esta página apresenta configurações completas e prontas para uso em diferentes cenários reais. Cada cenário inclui o `agent.yaml` e o `server.yaml` adequados.

---

## Índice

1. [Servidor único, backup simples](#1-servidor-único-backup-simples)
2. [Múltiplos diretórios para múltiplos storages](#2-múltiplos-diretórios-para-múltiplos-storages)
3. [Backup paralelo via WAN](#3-backup-paralelo-via-wan)
4. [Link saturado — throttling + parallel](#4-link-saturado--throttling--parallel)
5. [Servidor com SSD NVMe — alta performance](#5-servidor-com-ssd-nvme--alta-performance)
6. [Servidor com HDD/USB — baixa pressão de I/O](#6-servidor-com-hdusb--baixa-pressão-de-io)
7. [Múltiplos agents → único server](#7-múltiplos-agents--único-server)
8. [WebUI com persistência completa](#8-webui-com-persistência-completa)
9. [Backup noturno crítico com alta retenção](#9-backup-noturno-crítico-com-alta-retenção)
10. [Ambiente de desenvolvimento (baixa retenção, frequente)](#10-ambiente-de-desenvolvimento-baixa-retenção-frequente)

---

## 1. Servidor único, backup simples

**Cenário:** Um servidor faz backup de `/etc` e `/var/www` para o mesmo storage, via single stream, sem parallel, sem throttle.

### agent.yaml

```yaml
agent:
  name: "web-server-01"

server:
  address: "backup.example.com:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/agent.pem
  client_key: /etc/nbackup/agent-key.pem

backups:
  - name: config
    storage: configs
    schedule: "0 3 * * *"   # Diário às 03h
    sources:
      - path: /etc
    exclude:
      - "*.tmp"
      - "*.swp"

  - name: www
    storage: webroot
    schedule: "30 3 * * *"  # Diário às 03h30
    sources:
      - path: /var/www
    exclude:
      - "*.log"
      - "node_modules/**"
      - ".git/**"

retry:
  max_attempts: 5
  initial_delay: 1s
  max_delay: 5m

resume:
  buffer_size: 256mb

logging:
  level: info
  format: json
```

### server.yaml

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  configs:
    base_dir: /var/backups/configs
    max_backups: 14           # 2 semanas de histórico
    compression_mode: gzip
    assembler_mode: eager

  webroot:
    base_dir: /var/backups/webroot
    max_backups: 7            # 1 semana de histórico
    compression_mode: gzip
    assembler_mode: eager

logging:
  level: info
  format: json
```

---

## 2. Múltiplos diretórios para múltiplos storages

**Cenário:** Um servidor de banco de dados envia dumps para um storage dedicado, e os logs de aplicação para outro. Retenções diferentes por tipo.

### agent.yaml

```yaml
agent:
  name: "db-server-01"

server:
  address: "backup.example.com:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/agent.pem
  client_key: /etc/nbackup/agent-key.pem

backups:
  - name: db-dumps
    storage: databases
    schedule: "0 1 * * *"    # Diário às 01h — dumps noturnos
    sources:
      - path: /var/backups/mysql   # dumps gerados por script separado
    exclude:
      - "*.tmp"

  - name: app-logs
    storage: logs
    schedule: "0 */12 * * *" # A cada 12 horas
    sources:
      - path: /var/log/app
    exclude:
      - "*.gz"               # já comprimidos

retry:
  max_attempts: 3
  initial_delay: 2s
  max_delay: 10m

resume:
  buffer_size: 512mb         # dumps de DB podem ser grandes

logging:
  level: info
  format: json
```

### server.yaml

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  databases:
    base_dir: /var/backups/databases
    max_backups: 30           # 1 mês de dumps
    compression_mode: zst     # Zstandard — melhor razão para dados textuais
    assembler_mode: eager
    assembler_pending_mem_limit: 32mb

  logs:
    base_dir: /var/backups/logs
    max_backups: 7            # apenas 1 semana de logs
    compression_mode: gzip
    assembler_mode: lazy      # logs são sequenciais

logging:
  level: info
  format: json
```

---

## 3. Backup paralelo via WAN

**Cenário:** Escritório remoto com link WAN de 100 Mbps e latência ~30ms. Parallel streaming com AutoScaler adaptive para maximizar throughput no link instável.

### agent.yaml

```yaml
agent:
  name: "branch-office-sp"

server:
  address: "hq-backup.example.com:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/agent.pem
  client_key: /etc/nbackup/agent-key.pem

backups:
  - name: fileshare
    storage: branch-sp
    schedule: "0 22 * * *"   # Noturno às 22h (fora do horário de pico)
    parallels: 6              # Até 6 streams paralelos
    auto_scaler: adaptive     # Adaptive: probe-and-measure para WAN variável
    sources:
      - path: /srv/fileshare
    exclude:
      - "*.tmp"
      - "~$*"                 # arquivos temporários de Office
      - "Thumbs.db"
      - ".DS_Store"

retry:
  max_attempts: 8             # mais tentativas para link WAN instável
  initial_delay: 5s
  max_delay: 15m

resume:
  buffer_size: 512mb          # buffer maior para tolerar quedas de link WAN
  chunk_size: 2mb             # chunks maiores reduzem overhead de framing

daemon:
  control_channel:
    enabled: true
    keepalive_interval: 60s   # keepalive mais espaçado para WAN
    reconnect_delay: 10s
    max_reconnect_delay: 5m

logging:
  level: info
  format: json
```

### server.yaml

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  branch-sp:
    base_dir: /var/backups/branch-sp
    max_backups: 14
    compression_mode: zst
    assembler_mode: lazy      # lazy: estabilidade em links instáveis com muitos chunks
    chunk_shard_levels: 2     # 2 níveis: 6 streams geram muitos chunks

logging:
  level: info
  format: json
```

---

## 4. Link saturado — throttling + parallel

**Cenário:** Servidor compartilha link de uplink de 50 Mbps com outros serviços. O backup não pode ocupar mais que 20 MB/s para não impactar produção.

### agent.yaml

```yaml
agent:
  name: "app-server-shared-link"

server:
  address: "backup.example.com:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/agent.pem
  client_key: /etc/nbackup/agent-key.pem

backups:
  - name: app-data
    storage: app-data
    schedule: "0 2 * * *"
    parallels: 4              # parallel para latência, não para saturar o link
    auto_scaler: efficiency   # efficiency: estável em links com capacidade controlada
    bandwidth_limit: "20mb"   # 20 MB/s máximo — protege o uplink compartilhado
    sources:
      - path: /opt/app/data
    exclude:
      - "cache/**"
      - "*.log"

retry:
  max_attempts: 5
  initial_delay: 1s
  max_delay: 5m

resume:
  buffer_size: 256mb

logging:
  level: info
  format: json
```

### server.yaml

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  app-data:
    base_dir: /var/backups/app-data
    max_backups: 10
    compression_mode: zst
    assembler_mode: eager
    assembler_pending_mem_limit: 16mb
    chunk_shard_levels: 2

logging:
  level: info
  format: json
```

---

## 5. Servidor com SSD NVMe — alta performance

**Cenário:** Server de backup com NVMe rápido. Maximize throughput do assembler com eager mode e limite de memória alto.

### server.yaml

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  nvme-hot:
    base_dir: /var/backups/hot
    max_backups: 30
    compression_mode: zst
    assembler_mode: eager
    assembler_pending_mem_limit: 128mb  # Absorve grandes rajadas de chunks OOO
    chunk_shard_levels: 2               # 2 níveis para sessões paralelas intensas

  nvme-archive:
    base_dir: /var/backups/archive
    max_backups: 90                     # 3 meses de retenção
    compression_mode: zst
    assembler_mode: eager
    assembler_pending_mem_limit: 64mb

logging:
  level: info
  format: json
  stream_stats: false         # habilite com 'true' para diagnóstico de throughput
```

> **Nota:** Com NVMe, o gargalo costuma estar na rede. Use `parallels: 4-8` no agent e `auto_scaler: adaptive` para maximizar a utilização do link.

---

## 6. Servidor com HDD/USB — baixa pressão de I/O

**Cenário:** NAS ou storage externo USB/HDD com alta latência de I/O. Prefira `lazy` para reduzir seek aleatório durante a transferência.

### server.yaml

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  usb-archive:
    base_dir: /mnt/usb/backups
    max_backups: 7
    compression_mode: gzip    # gzip: menos CPU intensivo que zst
    assembler_mode: lazy      # lazy: zero I/O aleatório durante transferência
    # assembler_pending_mem_limit: ignorado em lazy

  nas-secondary:
    base_dir: /mnt/nas/backups
    max_backups: 14
    compression_mode: gzip
    assembler_mode: lazy
    chunk_shard_levels: 1     # 1 nível: NAS com filesystem limitado

logging:
  level: info
  format: json
```

> **Dica:** Com USB/HDD, configure no agent `bandwidth_limit` compatível com a velocidade real do dispositivo (ex: `20mb` para USB 3.0 comum).

---

## 7. Múltiplos agents → único server

**Cenário:** Data center com 5 servidores enviando backup ao mesmo server centralizado. Cada server tem seu próprio storage, e o server centraliza com WebUI habilitada.

### agent.yaml (servidor web — web-01)

```yaml
agent:
  name: "web-01"

server:
  address: "backup-central.dc.example.com:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/web-01.pem
  client_key: /etc/nbackup/web-01-key.pem

backups:
  - name: webroot
    storage: web-servers
    schedule: "0 2 * * *"
    parallels: 2
    sources:
      - path: /var/www
    exclude:
      - "*.log"
      - ".git/**"

retry:
  max_attempts: 5
  initial_delay: 1s
  max_delay: 5m

logging:
  level: info
  format: json
```

### server.yaml (backup centralizado)

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  web-servers:
    base_dir: /var/backups/web-servers   # web-01, web-02, etc. em subdiretórios automáticos
    max_backups: 7
    compression_mode: gzip
    assembler_mode: eager
    assembler_pending_mem_limit: 16mb
    chunk_shard_levels: 2

  db-servers:
    base_dir: /var/backups/db-servers
    max_backups: 30
    compression_mode: zst
    assembler_mode: lazy

  configs:
    base_dir: /var/backups/configs
    max_backups: 14
    compression_mode: gzip
    assembler_mode: eager

logging:
  level: info
  format: json

web_ui:
  enabled: true
  listen: "127.0.0.1:9848"
  allow_origins:
    - "10.10.0.0/16"           # rede interna do data center
    - "127.0.0.1/32"
  events_file: /var/lib/nbackup/events.jsonl
  events_max_lines: 10000
  session_history_file: /var/lib/nbackup/session-history.jsonl
  session_history_max_lines: 5000
  active_sessions_file: /var/lib/nbackup/active-sessions.jsonl
  active_snapshot_interval: 5m
```

> **Nota de organização:** O server cria automaticamente subdiretórios por agent. Com 5 agents no storage `web-servers`, a estrutura será:
> ```
> /var/backups/web-servers/
> ├── web-01/webroot/
> ├── web-02/webroot/
> ├── web-03/webroot/
> └── ...
> ```

---

## 8. WebUI com persistência completa

**Cenário:** Server em produção onde os dados da WebUI devem sobreviver a reinicios, com acesso restrito à equipe de DevOps.

### server.yaml (foco na WebUI)

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  primary:
    base_dir: /var/backups/primary
    max_backups: 14
    compression_mode: zst
    assembler_mode: eager
    assembler_pending_mem_limit: 32mb

logging:
  level: info
  format: json
  file: /var/log/nbackup/server.log

web_ui:
  enabled: true
  listen: "127.0.0.1:9848"    # Exposto via nginx reverse proxy com autenticação
  read_timeout: 5s
  write_timeout: 15s
  idle_timeout: 60s

  # Persistência — crie o diretório antes: mkdir -p /var/lib/nbackup
  events_file: /var/lib/nbackup/events.jsonl
  events_max_lines: 50000      # ~6 meses de eventos em operação normal
  session_history_file: /var/lib/nbackup/session-history.jsonl
  session_history_max_lines: 10000
  active_sessions_file: /var/lib/nbackup/active-sessions.jsonl
  active_sessions_max_lines: 5000
  active_snapshot_interval: 2m # snapshot mais frequente para diagnóstico

  allow_origins:
    - "10.0.0.0/8"             # rede DevOps
    - "172.16.0.0/12"          # rede de gestão
    - "127.0.0.1/32"
```

> **Reverse proxy (nginx):** Para expor a WebUI com autenticação básica:
> ```nginx
> location /nbackup/ {
>     proxy_pass http://127.0.0.1:9848/;
>     auth_basic "nbackup";
>     auth_basic_user_file /etc/nginx/.htpasswd;
> }
> ```

---

## 9. Backup noturno crítico com alta retenção

**Cenário:** Dados críticos de negócio com retenção de 90 dias, backup às 01h com verificação dupla de integridade (inerente ao protocolo SHA-256).

### agent.yaml

```yaml
agent:
  name: "erp-server"

server:
  address: "backup-critical.example.com:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/erp-server.pem
  client_key: /etc/nbackup/erp-server-key.pem

backups:
  - name: erp-data
    storage: critical
    schedule: "0 1 * * *"
    parallels: 4
    auto_scaler: efficiency
    sources:
      - path: /opt/erp/data
      - path: /opt/erp/attachments
    exclude:
      - "tmp/**"
      - "*.tmp"
      - "cache/**"

  - name: erp-config
    storage: critical-configs
    schedule: "30 1 * * *"    # 30min após o início dos dados
    sources:
      - path: /opt/erp/config
      - path: /etc/erp

retry:
  max_attempts: 10             # muitas tentativas para dados críticos
  initial_delay: 30s
  max_delay: 30m

resume:
  buffer_size: 1gb             # buffer grande para dados críticos volumosos
  chunk_size: 4mb

daemon:
  control_channel:
    enabled: true
    keepalive_interval: 30s
    reconnect_delay: 10s
    max_reconnect_delay: 10m

logging:
  level: info
  format: json
  file: /var/log/nbackup/agent.log
```

### server.yaml

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  critical:
    base_dir: /var/backups/critical
    max_backups: 90            # 3 meses de retenção diária
    compression_mode: zst
    assembler_mode: eager
    assembler_pending_mem_limit: 64mb
    chunk_shard_levels: 2

  critical-configs:
    base_dir: /var/backups/critical-configs
    max_backups: 90
    compression_mode: gzip
    assembler_mode: eager

logging:
  level: info
  format: json
  file: /var/log/nbackup/server.log
```

---

## 10. Ambiente de desenvolvimento (baixa retenção, frequente)

**Cenário:** Servidor de desenvolvimento com snapshots frequentes para garantir ponto de restauração antes de deploys. Alta frequência, baixa retenção.

### agent.yaml

```yaml
agent:
  name: "dev-server"

server:
  address: "backup-dev.internal:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/dev-server.pem
  client_key: /etc/nbackup/dev-server-key.pem

backups:
  - name: app-snapshot
    storage: dev-snapshots
    schedule: "0 */4 * * *"   # A cada 4 horas — restauração antes de deploys
    parallels: 0              # Single stream — dev não precisa de parallel
    sources:
      - path: /opt/app
    exclude:
      - ".git/**"
      - "node_modules/**"
      - "vendor/**"
      - "*.log"
      - "__pycache__/**"
      - ".venv/**"

  - name: db-snapshot
    storage: dev-databases
    schedule: "30 */4 * * *"  # 30min após o app
    sources:
      - path: /var/backups/pg-dumps   # dumps gerados por pg_dumpall

retry:
  max_attempts: 3             # dev pode falhar sem tanto impacto
  initial_delay: 5s
  max_delay: 2m

resume:
  buffer_size: 128mb          # menor buffer — dados de dev são menores

logging:
  level: debug                # debug em dev para troubleshooting
  format: text                # text é mais legível no terminal

daemon:
  control_channel:
    enabled: true
    keepalive_interval: 30s
```

### server.yaml

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  dev-snapshots:
    base_dir: /var/backups/dev/snapshots
    max_backups: 6             # Mantém 24h de histórico (6 × 4h)
    compression_mode: gzip
    assembler_mode: eager

  dev-databases:
    base_dir: /var/backups/dev/databases
    max_backups: 6
    compression_mode: zst
    assembler_mode: lazy

logging:
  level: debug                 # debug para facilitar diagnóstico em dev
  format: text

web_ui:
  enabled: true
  listen: "0.0.0.0:9848"     # aberto na rede interna de dev
  allow_origins:
    - "192.168.100.0/24"      # rede de dev
    - "127.0.0.1/32"
```

---

## 11. HDD/NAS lento com Chunk Buffer

**Cenário:** Server de backup com HDD ou NAS de alta latência de escrita. O `chunk_buffer` absorve os chunks recebidos em memória enquanto o disco "digere" os escritos em lote, mantendo a rede ativa sem esperar cada flush.

### Quando usar

- Discos rotativos (HDD 5400-7200 RPM)
- NAS via rede (SMB/NFS com latência > 5ms por operação)
- USB 3.0 externo com throughput variável
- Qualquer armazenamento onde `iostat` mostre `%util` alto com baixo throughput

### agent.yaml

```yaml
agent:
  name: "app-server-hdd"

server:
  address: "backup.example.com:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/agent.pem
  client_key: /etc/nbackup/agent-key.pem

backups:
  - name: app-data
    storage: hdd-archive
    schedule: "0 2 * * *"
    parallels: 2               # Parallel moderado — HDD não ganha com muitos streams simultâneos
    auto_scaler: efficiency
    bandwidth_limit: "30mb"    # Alinha com throughput real do HDD
    sources:
      - path: /opt/app/data
    exclude:
      - "tmp/**"
      - "cache/**"
      - "*.log"

retry:
  max_attempts: 5
  initial_delay: 1s
  max_delay: 5m

resume:
  buffer_size: 256mb
  chunk_size: 1mb

logging:
  level: info
  format: json
```

### server.yaml

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

storages:
  hdd-archive:
    base_dir: /mnt/hdd/backups
    max_backups: 14
    compression_mode: gzip      # gzip: menos intensivo em CPU que zst para HDD lento
    assembler_mode: lazy        # lazy: zero I/O aleatório durante a transferência
    chunk_shard_levels: 1       # 1 nível: HDD não se beneficia de diretórios extras

logging:
  level: info
  format: json

# chunk_buffer absorve spikes de rede enquanto o HDD "digere" os escritos.
# Dimensionado para cobrir ~4s de ingestão a 30 MB/s = 120 MB mínimo.
chunk_buffer:
  size: 128mb      # Reservado no startup — ajuste conforme RAM disponível
  drain_ratio: 0.5 # Drena quando atingir 50% — mantém HDD ocupado sem backpressure
```

> **Por que `drain_ratio: 0.5`?** Com HDD, escrever cedo (low ratio) gera seek aleatório durante a ingestão. Escrever tarde (high ratio) atrasa demais. 50% é o equilíbrio: o buffer acumula rajadas de rede e drena em lote para o disco.

> **Indicadores de que o buffer está ajudado:** Throughput de rede estável nos sparklines da WebUI sem quedas abruptas durante flush de disco.

---

## Referências

- [[Configuração de Exemplo|Configuracao-de-Exemplo]] — Referência completa de todos os campos
- [[Guia de Uso|Guia-de-Uso]] — Comandos, retry, resume, parallel streaming, chunk_buffer, dscp
- [[Arquitetura]] — Detalhes internos do pipeline, assembler e control channel
- [[Especificação Técnica|Especificacao-Tecnica]] — Protocolo binário e frames
