# Guia de Instalação

## Pré-requisitos

| Requisito | Versão Mínima |
|-----------|---------------|
| Go | 1.21+ |
| OpenSSL (ou similar) | Para geração de certificados |
| Linux (amd64/arm64) | Produção |

---

## 1. Build dos Binários

```bash
git clone https://github.com/nishisan-dev/n-backup.git
cd n-backup

# Compilar ambos os binários
go build -o bin/nbackup-agent ./cmd/nbackup-agent
go build -o bin/nbackup-server ./cmd/nbackup-server
```

Para cross-compilation (ex: build em macOS para deploy em Linux):

```bash
GOOS=linux GOARCH=amd64 go build -o bin/nbackup-agent ./cmd/nbackup-agent
GOOS=linux GOARCH=amd64 go build -o bin/nbackup-server ./cmd/nbackup-server
```

---

## 2. Geração de Certificados (mTLS)

O n-backup exige **mutual TLS** — tanto o server quanto o agent precisam de certificados assinados pela mesma CA.

### 2.1. Criar a CA (Certificate Authority)

```bash
# Gera chave privada da CA
openssl ecparam -genkey -name prime256v1 -out ca-key.pem

# Cria certificado da CA (valido por 10 anos)
openssl req -new -x509 -sha256 -key ca-key.pem \
  -out ca.pem -days 3650 \
  -subj "/CN=NBackup CA"
```

### 2.2. Criar Certificado do Server

```bash
# Gera chave privada
openssl ecparam -genkey -name prime256v1 -out server-key.pem

# Gera CSR
openssl req -new -sha256 -key server-key.pem \
  -out server.csr \
  -subj "/CN=nbackup-server"

# Cria extensões (SANs)
cat > server-ext.cnf << EOF
subjectAltName = DNS:backup.nishisan.dev, DNS:localhost, IP:127.0.0.1
extendedKeyUsage = serverAuth
EOF

# Assina com a CA
openssl x509 -req -sha256 -in server.csr \
  -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -out server.pem -days 365 \
  -extfile server-ext.cnf
```

### 2.3. Criar Certificado do Agent

Repita para **cada agent** com um CN único:

```bash
AGENT_NAME="web-server-01"

openssl ecparam -genkey -name prime256v1 -out ${AGENT_NAME}-key.pem

openssl req -new -sha256 -key ${AGENT_NAME}-key.pem \
  -out ${AGENT_NAME}.csr \
  -subj "/CN=${AGENT_NAME}"

cat > agent-ext.cnf << EOF
extendedKeyUsage = clientAuth
EOF

openssl x509 -req -sha256 -in ${AGENT_NAME}.csr \
  -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -out ${AGENT_NAME}.pem -days 365 \
  -extfile agent-ext.cnf
```

### 2.4. Distribuir os Certificados

**No Server:**

```
/etc/nbackup/
├── ca.pem            # CA
├── server.pem        # Cert do server
└── server-key.pem    # Chave do server
```

**Em cada Agent:**

```
/etc/nbackup/
├── ca.pem            # CA (mesma)
├── agent.pem         # Cert do agent
└── agent-key.pem     # Chave do agent
```

> [!IMPORTANT]
> As chaves privadas devem ter permissão restrita: `chmod 600 *-key.pem`

---

## 3. Configuração

### 3.1. Server

```bash
sudo mkdir -p /etc/nbackup /var/backups/scripts /var/backups/home
sudo cp configs/server.example.yaml /etc/nbackup/server.yaml
```

Edite `/etc/nbackup/server.yaml`:

```yaml
server:
  listen: "0.0.0.0:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  server_cert: /etc/nbackup/server.pem
  server_key: /etc/nbackup/server-key.pem

# Named storages — cada storage tem seu próprio diretório e rotação
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

logging:
  level: info
  format: json
```

Defaults dos novos campos por storage:
- `assembler_mode: eager`
- `assembler_pending_mem_limit: 8mb` (8 * 1024 * 1024 bytes)

### 3.2. Agent

```bash
sudo mkdir -p /etc/nbackup
sudo cp configs/agent.example.yaml /etc/nbackup/agent.yaml
```

Edite `/etc/nbackup/agent.yaml`:

```yaml
agent:
  name: "web-server-01"        # Identifica este host

daemon:
  schedule: "0 2 * * *"        # Cron: diário às 02h

server:
  address: "backup.nishisan.dev:9847"

tls:
  ca_cert: /etc/nbackup/ca.pem
  client_cert: /etc/nbackup/agent.pem
  client_key: /etc/nbackup/agent-key.pem

# Cada entry é um backup independente, direcionado a um storage nomeado no server
backups:
  - name: app
    storage: scripts           # Nome do storage no server
    sources:
      - path: /app/scripts
    exclude:
      - "*.log"

  - name: home
    storage: home-dirs
    sources:
      - path: /home
    exclude:
      - ".git/**"
      - "node_modules/**"

retry:
  max_attempts: 5
  initial_delay: 1s
  max_delay: 5m

logging:
  level: info
  format: json
```

---

## 4. Deploy dos Binários

```bash
sudo cp bin/nbackup-server /usr/local/bin/
sudo cp bin/nbackup-agent /usr/local/bin/
```

---

## 5. Systemd (Produção)

### 5.1. Server

```bash
sudo tee /etc/systemd/system/nbackup-server.service << 'EOF'
[Unit]
Description=NBackup Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/nbackup-server --config /etc/nbackup/server.yaml
Restart=on-failure
RestartSec=5s
User=nbackup
Group=nbackup
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

sudo useradd -r -s /usr/sbin/nologin nbackup
sudo chown -R nbackup:nbackup /var/backups/nbackup /etc/nbackup
sudo systemctl daemon-reload
sudo systemctl enable --now nbackup-server
```

### 5.2. Agent

```bash
sudo tee /etc/systemd/system/nbackup-agent.service << 'EOF'
[Unit]
Description=NBackup Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/nbackup-agent --config /etc/nbackup/agent.yaml
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now nbackup-agent
```

---

## 6. Verificação

```bash
# Verifica status do server
sudo systemctl status nbackup-server

# Verifica status do agent
sudo systemctl status nbackup-agent

# Health check remoto
nbackup-agent health backup.nishisan.dev:9847 --config /etc/nbackup/agent.yaml

# Testar backup manualmente
nbackup-agent --config /etc/nbackup/agent.yaml --once
```

---

## 7. Estrutura de Storage (Server)

Cada storage nomeado tem seu próprio diretório base. Dentro dele, os backups são organizados por agent:

```
/var/backups/scripts/              ← storage "scripts"
├── web-server-01/
│   ├── 2026-02-10T02-00-00.tar.gz
│   └── 2026-02-12T02-00-00.tar.gz
└── web-server-02/
    └── 2026-02-12T02-00-00.tar.gz

/var/backups/home/                 ← storage "home-dirs"
├── web-server-01/
│   ├── 2026-02-10T02-00-00.tar.gz
│   └── 2026-02-11T02-00-00.tar.gz
└── ...
```

Cada storage tem sua própria configuração de `max_backups` para rotação independente.
