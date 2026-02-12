# n-backup — Plano de Implementação Faseado

## Contexto

O projeto **n-backup** é um sistema de backup client-server em Go. A especificação técnica completa já foi definida e aprovada em [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md). Este plano organiza a implementação em **7 fases**, partindo das camadas mais fundamentais (protocolo, config, TLS) até o daemon completo e testes end-to-end.

> [!IMPORTANT]
> Cada fase será implementada, testada e commitada atomicamente antes de prosseguir para a seguinte. Isso permite rollback cirúrgico por fase.

---

## Estrutura Final do Projeto

```
n-backup/
├── cmd/
│   ├── nbackup-agent/main.go
│   └── nbackup-server/main.go
├── internal/
│   ├── config/
│   │   ├── agent.go
│   │   ├── server.go
│   │   └── config_test.go
│   ├── logging/
│   │   └── logger.go
│   ├── protocol/
│   │   ├── frames.go
│   │   ├── reader.go
│   │   ├── writer.go
│   │   └── protocol_test.go
│   ├── pki/
│   │   ├── tls.go
│   │   └── tls_test.go
│   ├── agent/
│   │   ├── scanner.go
│   │   ├── streamer.go
│   │   ├── backup.go
│   │   ├── scheduler.go
│   │   ├── daemon.go
│   │   └── agent_test.go
│   └── server/
│       ├── handler.go
│       ├── storage.go
│       ├── rotation.go
│       ├── server.go
│       └── server_test.go
├── configs/
├── docs/
├── planning/
├── go.mod
├── go.sum
└── README.md
```

---

## Fase 1 — Scaffolding, Config e Logging

**Objetivo:** Criar a estrutura base do projeto Go, parsing de configuração YAML e logger estruturado.

### Dependências Externas
- `gopkg.in/yaml.v3` — parsing YAML
- `github.com/robfig/cron/v3` — scheduler (declarada agora, usada na Fase 6)

---

### [NEW] [go.mod](file:///home/lucas/Projects/n-backup/go.mod)

Inicializar com `go mod init github.com/nishisan-dev/n-backup`. Go 1.22+.

---

### [NEW] [agent.go](file:///home/lucas/Projects/n-backup/internal/config/agent.go)

Structs para o config do agent, mapeando toda a estrutura do `agent.example.yaml`:

```go
type AgentConfig struct {
    Agent   AgentInfo   `yaml:"agent"`
    Daemon  DaemonInfo  `yaml:"daemon"`
    Server  ServerInfo  `yaml:"server"`
    TLS     TLSClient   `yaml:"tls"`
    Backup  BackupInfo  `yaml:"backup"`
    Retry   RetryInfo   `yaml:"retry"`
    Logging LoggingInfo `yaml:"logging"`
}
```

- `LoadAgentConfig(path string) (*AgentConfig, error)` — lê e valida o YAML.
- Validação: `name` não vazio, `schedule` parseable, paths TLS existem, pelo menos um source.

---

### [NEW] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

Mesmo padrão para config do server:

```go
type ServerConfig struct {
    Server  ServerListen `yaml:"server"`
    TLS     TLSServer    `yaml:"tls"`
    Storage StorageInfo  `yaml:"storage"`
    Logging LoggingInfo  `yaml:"logging"`
}
```

- `LoadServerConfig(path string) (*ServerConfig, error)`
- Validação: `listen` não vazio, `base_dir` existe ou pode ser criado, `max_backups >= 1`.

---

### [NEW] [config_test.go](file:///home/lucas/Projects/n-backup/internal/config/config_test.go)

Testes unitários:
- Parse bem-sucedido dos dois YAMLs de exemplo (`configs/agent.example.yaml`, `configs/server.example.yaml`)
- Validação de campos obrigatórios (falha se `name` vazio, se `schedule` inválido, etc.)
- Valores default onde aplicável

---

### [NEW] [logger.go](file:///home/lucas/Projects/n-backup/internal/logging/logger.go)

Factory para `slog.Logger` baseado na config YAML:

```go
func NewLogger(level string, format string) *slog.Logger
```

- `format: "json"` → `slog.NewJSONHandler(os.Stdout, opts)`
- `format: "text"` → `slog.NewTextHandler(os.Stdout, opts)`
- `level` mapeado para `slog.Level`

---

## Fase 2 — Protocolo Binário

**Objetivo:** Implementar a camada de protocolo conforme a seção 3 da spec (frames, serialização/deserialização).

---

### [NEW] [frames.go](file:///home/lucas/Projects/n-backup/internal/protocol/frames.go)

Constantes e tipos:

```go
const (
    MagicHandshake = "NBKP"
    MagicTrailer   = "DONE"
    MagicPing      = "PING"
    ProtocolVersion = 0x01
)

// ACK status codes
const (
    StatusGo             = 0x00
    StatusFull           = 0x01
    StatusBusy           = 0x02
    StatusReject         = 0x03
)

// Final ACK status codes
const (
    FinalStatusOK               = 0x00
    FinalStatusChecksumMismatch = 0x01
    FinalStatusWriteError       = 0x02
)
```

Structs: `Handshake`, `ACK`, `Trailer`, `FinalACK`, `HealthResponse`.

---

### [NEW] [writer.go](file:///home/lucas/Projects/n-backup/internal/protocol/writer.go)

Funções de escrita sobre `io.Writer`:

- `WriteHandshake(w io.Writer, agentName string) error`
- `WriteTrailer(w io.Writer, checksum [32]byte, size uint64) error`
- `WriteACK(w io.Writer, status byte, message string) error`
- `WriteFinalACK(w io.Writer, status byte) error`
- `WritePing(w io.Writer) error`
- `WriteHealthResponse(w io.Writer, status byte, diskFree uint64) error`

Encoding: `binary.BigEndian` para campos numéricos, UTF-8 para strings, delimitado por `\n`.

---

### [NEW] [reader.go](file:///home/lucas/Projects/n-backup/internal/protocol/reader.go)

Funções de leitura sobre `io.Reader`:

- `ReadHandshake(r io.Reader) (*Handshake, error)` — valida magic + version
- `ReadTrailer(r io.Reader) (*Trailer, error)` — lê magic "DONE" + SHA-256 + size
- `ReadACK(r io.Reader) (*ACK, error)`
- `ReadFinalACK(r io.Reader) (*FinalACK, error)`
- `ReadPing(r io.Reader) error` — valida magic "PING"
- `ReadHealthResponse(r io.Reader) (*HealthResponse, error)`

---

### [NEW] [protocol_test.go](file:///home/lucas/Projects/n-backup/internal/protocol/protocol_test.go)

Testes unitários com `bytes.Buffer` como transport simulado:
- Round-trip: Write → Read para cada frame type
- Magic inválido → erro
- Versão incompatível → erro
- Campos truncados → erro

---

## Fase 3 — PKI e TLS

**Objetivo:** Carregamento de certificados e configuração de `tls.Config` para cliente (mTLS client) e servidor (mTLS server).

---

### [NEW] [tls.go](file:///home/lucas/Projects/n-backup/internal/pki/tls.go)

Duas funções públicas:

```go
// NewClientTLSConfig carrega CA + client cert/key para mTLS client
func NewClientTLSConfig(caCert, clientCert, clientKey string) (*tls.Config, error)

// NewServerTLSConfig carrega CA + server cert/key para mTLS server
func NewServerTLSConfig(caCert, serverCert, serverKey string) (*tls.Config, error)
```

- TLS 1.3 mínimo (`MinVersion: tls.VersionTLS13`)
- Client: `tls.RequireAndVerifyClientCert` no server-side
- Server: `tls.RequireAndVerifyClientCert` + CA pool carregada
- Ambos carregam `tls.LoadX509KeyPair` e `x509.CertPool`

---

### [NEW] [tls_test.go](file:///home/lucas/Projects/n-backup/internal/pki/tls_test.go)

- Gerar certificados self-signed em memória para teste (usando `crypto/x509` + `crypto/ecdsa`)
- Testar conexão mTLS client ↔ server via `net.Pipe` + `tls.Client`/`tls.Server`
- Testar rejeição quando certificado do client não é assinado pela CA

---

## Fase 4 — Agent: Pipeline de Streaming

**Objetivo:** Implementar o pipeline completo `fs.WalkDir → tar → gzip → checksum → connection`, incluindo glob matching.

---

### [NEW] [scanner.go](file:///home/lucas/Projects/n-backup/internal/agent/scanner.go)

```go
// Scanner caminha pelos sources e filtra com exclude/include
type Scanner struct {
    sources  []string
    excludes []string
}

// Scan itera sobre os arquivos e envia cada entry para o callback
func (s *Scanner) Scan(ctx context.Context, fn func(path string, info fs.DirEntry) error) error
```

- `fs.WalkDir` sobre cada source
- `filepath.Match` ou `doublestar` para glob patterns
- Respeita `ctx.Done()` para cancelamento
- Symlinks: seguir no tar (preservar como symlink entry)

---

### [NEW] [streamer.go](file:///home/lucas/Projects/n-backup/internal/agent/streamer.go)

Pipeline de streaming zero-copy:

```go
// Stream executa o pipeline completo e retorna checksum e bytes enviados
func Stream(ctx context.Context, scanner *Scanner, conn io.Writer) (checksum [32]byte, size uint64, err error)
```

Internamente:
1. `io.Pipe()` → `pr, pw`
2. Goroutine: `tar.NewWriter(pw)` → itera scanner → `tw.WriteHeader` + `io.Copy` → `tw.Close()` → `pw.Close()`
3. Main: `gzip.NewWriter(hashAndConn)` onde `hashAndConn = io.MultiWriter(sha256hasher, conn)`
4. `io.Copy(gzipWriter, pr)`
5. Retorna `hasher.Sum(nil)` e bytes contados

---

### [NEW] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

Orquestra uma sessão de backup completa:

```go
func RunBackup(ctx context.Context, cfg *config.AgentConfig, logger *slog.Logger) error
```

1. Carrega TLS config via `pki.NewClientTLSConfig`
2. `tls.Dial` para o server
3. `protocol.WriteHandshake` → `protocol.ReadACK` (trata FULL/BUSY/REJECT)
4. `Stream(ctx, scanner, conn)` → obtém checksum + size
5. `protocol.WriteTrailer` → `protocol.ReadFinalACK`
6. Fecha conexão
7. Retorna erro se qualquer etapa falhar

---

### [NEW] [agent_test.go](file:///home/lucas/Projects/n-backup/internal/agent/agent_test.go)

- **Scanner test:** criar `t.TempDir()` com estrutura de arquivos, testar include/exclude
- **Streamer test:** verificar que o output é um `.tar.gz` válido, checksum confere, conteúdo extraível
- **Pipeline integration mock:** `net.Pipe` simulando server que lê handshake → envia ACK → consome stream → lê trailer → verifica checksum → envia FinalACK

---

## Fase 5 — Server: Receptor, Storage e Rotação

**Objetivo:** Implementar o server que aceita conexões, recebe o stream, grava atomicamente e faz rotação.

---

### [NEW] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

Trata uma conexão individual:

```go
func HandleConnection(ctx context.Context, conn net.Conn, cfg *config.ServerConfig, logger *slog.Logger) error
```

1. `protocol.ReadHandshake` → valida magic, version
2. Verifica lock (agent já em backup? → BUSY)
3. Verifica espaço em disco → FULL se insuficiente
4. `protocol.WriteACK(GO)`
5. Cria arquivo `.tmp` em `base_dir/<agent_name>/`
6. `io.Copy` com `io.MultiWriter(file, sha256hasher)` — stream → disco + checksum
7. `protocol.ReadTrailer` → compara checksums
8. Se OK: atomic rename `.tmp` → `<timestamp>.tar.gz` + rotação
9. `protocol.WriteFinalACK`

---

### [NEW] [storage.go](file:///home/lucas/Projects/n-backup/internal/server/storage.go)

```go
// AtomicWrite gerencia escrita em .tmp e rename atômico
type AtomicWriter struct { ... }

func NewAtomicWriter(baseDir, agentName string) (*AtomicWriter, error)
func (w *AtomicWriter) TempFile() (*os.File, error)
func (w *AtomicWriter) Commit(tmpPath string) (finalPath string, err error)  // rename
func (w *AtomicWriter) Abort(tmpPath string) error  // remove .tmp
```

- Filename final: `<timestamp-RFC3339>.tar.gz` (ex: `2026-02-11T02:00:00.tar.gz`)
- Cria diretório do agent se não existir (`os.MkdirAll`)

---

### [NEW] [rotation.go](file:///home/lucas/Projects/n-backup/internal/server/rotation.go)

```go
// Rotate remove backups excedentes, mantendo os max_backups mais recentes
func Rotate(agentDir string, maxBackups int) error
```

- Lista arquivos `.tar.gz` no diretório do agent
- Ordena por nome (timestamp → ordem natural)
- Remove os mais antigos que excedam `maxBackups`

---

### [NEW] [server.go](file:///home/lucas/Projects/n-backup/internal/server/server.go)

Loop principal do server:

```go
func Run(ctx context.Context, cfg *config.ServerConfig, logger *slog.Logger) error
```

1. Carrega TLS config via `pki.NewServerTLSConfig`
2. `tls.Listen` na porta configurada
3. Loop: `Accept` → goroutine `HandleConnection`
4. Graceful shutdown via `ctx.Done()`
5. Mapa de locks por agent name (`sync.Map`) para prevenir backups simultâneos do mesmo agent

---

### [NEW] [server_test.go](file:///home/lucas/Projects/n-backup/internal/server/server_test.go)

- **Rotation test:** criar N arquivos em `t.TempDir()`, chamar `Rotate(dir, 3)`, verificar que apenas 3 permanecem
- **AtomicWriter test:** Write .tmp → Commit → verificar arquivo final existe, .tmp não existe
- **AtomicWriter abort test:** Write .tmp → Abort → verificar .tmp removido
- **Handler test:** mock connection via `net.Pipe`, simular handshake completo + data + trailer

---

## Fase 6 — Agent: Daemon, Scheduler e CLI

**Objetivo:** Implementar o modo daemon com scheduler cron, retry com exponential backoff, graceful shutdown, e os entrypoints CLI.

---

### [NEW] [scheduler.go](file:///home/lucas/Projects/n-backup/internal/agent/scheduler.go)

```go
type Scheduler struct {
    cron     *cron.Cron
    logger   *slog.Logger
    backupFn func(ctx context.Context) error
    mu       sync.Mutex  // apenas um backup por vez
}

func NewScheduler(schedule string, logger *slog.Logger, fn func(ctx context.Context) error) (*Scheduler, error)
func (s *Scheduler) Start()
func (s *Scheduler) Stop(ctx context.Context)
```

- Usa `robfig/cron/v3`
- Mutex interno garante exclusão mútua (se cron trigger chegar durante backup ativo → skip com log warning)

---

### [NEW] [daemon.go](file:///home/lucas/Projects/n-backup/internal/agent/daemon.go)

```go
func RunDaemon(cfg *config.AgentConfig, logger *slog.Logger) error
```

1. Cria `Scheduler` com a cron expression do config
2. Registra signal handler para `SIGTERM` e `SIGINT`
3. Inicia scheduler
4. Bloqueia até signal → graceful shutdown (aguarda backup em andamento ou timeout)

---

### Retry (exponential backoff) — incluído em [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

Wrapper `RunBackupWithRetry`:

```go
func RunBackupWithRetry(ctx context.Context, cfg *config.AgentConfig, logger *slog.Logger) error
```

- `max_attempts`, `initial_delay`, `max_delay` do config
- Delay = `min(initial_delay * 2^attempt, max_delay)`
- Apenas retry em erros de conexão; mid-stream failure → abort (sem retry do stream, reagendar)

---

### [NEW] [main.go](file:///home/lucas/Projects/n-backup/cmd/nbackup-agent/main.go)

CLI do agent:

```
nbackup-agent --config <path>           → daemon mode
nbackup-agent --config <path> --once    → execução única
nbackup-agent health <server:port>      → health check
```

- Flag parsing com `flag` stdlib
- Subcomando `health` detectado via `os.Args`

---

### [NEW] [main.go](file:///home/lucas/Projects/n-backup/cmd/nbackup-server/main.go)

CLI do server:

```
nbackup-server --config <path>
```

- Carrega config → cria logger → chama `server.Run()`

---

## Fase 7 — Integração e Testes End-to-End

**Objetivo:** Validar o fluxo completo agent → server com certificados reais gerados em teste.

---

### Teste de integração end-to-end

1. Gerar PKI completa em `t.TempDir()` (CA → server cert → agent cert)
2. Criar diretório de origem com arquivos-teste variados (texto, binário, symlinks, diretórios vazios)
3. Iniciar server em goroutine (porta efêmera `:0`)
4. Configurar agent para backup `--once` apontando para o server local
5. Executar backup
6. Verificar:
   - Arquivo `.tar.gz` existe no storage do server
   - Conteúdo extraível e idêntico ao original
   - Rotação funciona (executar N+1 backups com `max_backups=N`)
7. Testar health check na mesma PKI

### Teste de cenários de erro

- Server indisponível → retry com backoff
- Checksum mismatch (corromper stream) → FinalACK MISMATCH + arquivo .tmp removido
- Agent name duplicado simultâneo → BUSY
- Certificado inválido → rejeição TLS

---

## Verificação

### Testes Automatizados

```bash
# Rodar todos os testes unitários e de integração
cd /home/lucas/Projects/n-backup
go test ./... -v -count=1

# Rodar apenas testes de uma fase específica (exemplo: protocolo)
go test ./internal/protocol/... -v -count=1

# Rodar com race detector
go test ./... -v -race -count=1

# Compilar os binários para verificar que não há erros de compilação
go build ./cmd/nbackup-agent/
go build ./cmd/nbackup-server/
```

### Verificação Manual (pós Fase 7)

1. **Gerar certificados de teste:**
   ```bash
   # Usar o script/teste que gera PKI em t.TempDir() e copiar para /tmp/nbackup-test/
   ```

2. **Iniciar o server:**
   ```bash
   ./nbackup-server --config configs/server.example.yaml
   ```

3. **Executar backup único:**
   ```bash
   ./nbackup-agent --config configs/agent.example.yaml --once
   ```

4. **Health check:**
   ```bash
   ./nbackup-agent health localhost:9847
   ```

5. **Validar o arquivo gerado:**
   ```bash
   tar tzf /var/backups/nbackup/<agent-name>/<timestamp>.tar.gz
   ```

> [!TIP]
> A verificação manual completa depende de certificados mTLS válidos. Na primeira execução, recomendo criarmos um script helper `scripts/gen-test-certs.sh` para gerar a PKI de teste facilmente.

---

## Ordem de Execução e Commits

| Fase | Escopo | Commit Message |
|------|--------|----------------|
| 1 | Scaffolding + Config + Logging | `feat: add project scaffolding, config parsing, and structured logging` |
| 2 | Protocolo Binário | `feat: implement binary protocol (frames, reader, writer)` |
| 3 | PKI / TLS | `feat: add mTLS config loading (client and server)` |
| 4 | Agent Pipeline | `feat: implement agent streaming pipeline (scanner, tar, gzip, checksum)` |
| 5 | Server Receptor + Storage | `feat: implement server receiver, atomic storage, and rotation` |
| 6 | Daemon + Scheduler + CLI | `feat: add daemon mode, cron scheduler, retry, and CLI entrypoints` |
| 7 | Integração E2E | `test: add end-to-end integration tests` |
