# SPA de Observabilidade — Plano de Implementação

Interface web (SPA) embutida no binário do `nbackup-server` para observabilidade operacional: sessões ativas, throughput por stream, eventos recentes e config efetiva. Listener HTTP dedicado com ACL por IP/CIDR, sem autenticação na v1.

## User Review Required

> [!IMPORTANT]
> **Porta padrão `9848`** — confirmar que não conflita com serviços existentes.

> [!IMPORTANT]
> **Sem autenticação na v1** — segurança depende exclusivamente de ACL + bind interno.

> [!WARNING]
> **Acesso read-only ao `Handler`** — a SPA precisa de referência ao Handler para coletar métricas. Isso requer alterar a assinatura de `Run`/`RunWithListener` ou expor o `Handler` criado internamente.

---

## Proposta de Alterações

### Configuração & Config Parse

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/config/server.go)

Adicionar struct `WebUIConfig` e campo `WebUI WebUIConfig` em `ServerConfig`:

```go
type WebUIConfig struct {
    Enabled      bool          `yaml:"enabled"`
    Listen       string        `yaml:"listen"`
    ReadTimeout  time.Duration `yaml:"read_timeout"`
    WriteTimeout time.Duration `yaml:"write_timeout"`
    IdleTimeout  time.Duration `yaml:"idle_timeout"`
    AllowOrigins []string      `yaml:"allow_origins"` // IP ou CIDR
}
```

Adicionar validação em `validate()`:
- Default `Listen = "127.0.0.1:9848"`
- Default timeouts: Read=5s, Write=15s, Idle=60s
- Parse de cada `AllowOrigins` como `net.ParseCIDR` ou IP único
- `AllowOrigins` vazio quando `Enabled=true` → erro (deny-by-default seguro)

#### [MODIFY] [server.example.yaml](file:///home/lucas/Projects/n-backup/configs/server.example.yaml)

Adicionar bloco `web_ui` ao exemplo:

```yaml
web_ui:
  enabled: false
  listen: "127.0.0.1:9848"
  read_timeout: 5s
  write_timeout: 15s
  idle_timeout: 60s
  allow_origins:
    - "127.0.0.1/32"
    # - "10.0.0.0/8"
```

---

### ACL Middleware

#### [NEW] [acl.go](file:///home/lucas/Projects/n-backup/internal/server/observability/acl.go)

Middleware HTTP que:
1. Extrai IP remoto de `r.RemoteAddr` (split host:port)
2. Verifica contra lista de `net.IPNet` parseadas no init
3. Retorna `403 Forbidden` se não permitido

```go
type ACL struct {
    nets []*net.IPNet
}

func NewACL(origins []string) (*ACL, error) // parse CIDR ou IP/32
func (a *ACL) Middleware(next http.Handler) http.Handler
```

#### [NEW] [acl_test.go](file:///home/lucas/Projects/n-backup/internal/server/observability/acl_test.go)

Tabela de testes:
- IP permitido → 200
- IP negado → 403
- CIDR range → permitido/negado
- IP sem porta → parsing correto
- Lista vazia → nega tudo

---

### Pacote Observability — Snapshot & HTTP

#### [NEW] [dto.go](file:///home/lucas/Projects/n-backup/internal/server/observability/dto.go)

DTOs JSON para cada endpoint:

```go
type HealthResponse struct {
    Status  string `json:"status"`   // "ok"
    Uptime  string `json:"uptime"`
    Version string `json:"version"`
}

type MetricsResponse struct {
    TrafficInMBps  float64 `json:"traffic_in_mbps"`
    DiskWriteMBps  float64 `json:"disk_write_mbps"`
    ActiveConns    int32   `json:"active_conns"`
    ActiveSessions int     `json:"active_sessions"`
}

type SessionSummary struct {
    SessionID     string  `json:"session_id"`
    Agent         string  `json:"agent"`
    Storage       string  `json:"storage"`
    Backup        string  `json:"backup,omitempty"`
    Mode          string  `json:"mode"` // single | parallel
    StartedAt     string  `json:"started_at"`
    LastActivity  string  `json:"last_activity"`
    BytesReceived int64   `json:"bytes_received"`
    ActiveStreams  int     `json:"active_streams"`
    MaxStreams     int     `json:"max_streams,omitempty"`
    Status        string  `json:"status"` // running | idle | degraded
}

type SessionDetail struct {
    SessionSummary
    Streams []StreamDetail `json:"streams,omitempty"`
}

type StreamDetail struct {
    Index       uint8   `json:"index"`
    OffsetBytes int64   `json:"offset_bytes"`
    MBps        float64 `json:"mbps"`
    IdleSecs    int64   `json:"idle_secs"`
    SlowSince   string  `json:"slow_since,omitempty"`
}

type EventEntry struct {
    Timestamp string `json:"timestamp"`
    Level     string `json:"level"` // info | warn | error
    Type      string `json:"type"`  // reconnect | rotate | stream_dead | checksum_mismatch
    Agent     string `json:"agent,omitempty"`
    Stream    int    `json:"stream,omitempty"`
    Message   string `json:"message"`
}

type ConfigEffective struct {
    ServerListen   string                 `json:"server_listen"`
    WebUIListen    string                 `json:"web_ui_listen"`
    Storages       map[string]interface{} `json:"storages"`
    FlowRotation   interface{}            `json:"flow_rotation"`
    LogLevel       string                 `json:"log_level"`
}
```

#### [NEW] [snapshot.go](file:///home/lucas/Projects/n-backup/internal/server/observability/snapshot.go)

Funções que recebem `*server.Handler` e leem dados via atômicos/sync.Map sem locks pesados:

```go
type Snapshotter struct {
    handler   *server.Handler
    cfg       *config.ServerConfig
    startTime time.Time
    events    *EventRing
}

func NewSnapshotter(handler *server.Handler, cfg *config.ServerConfig, events *EventRing) *Snapshotter
func (s *Snapshotter) Health() HealthResponse
func (s *Snapshotter) Metrics() MetricsResponse
func (s *Snapshotter) Sessions() []SessionSummary
func (s *Snapshotter) Session(id string) (*SessionDetail, bool)
func (s *Snapshotter) ConfigEffective() ConfigEffective
```

> [!NOTE]
> O `Snapshotter` precisa acesso read-only ao `Handler`. Para viabilizar isso, os campos `sessions *sync.Map`, `TrafficIn`, `DiskWrite`, `ActiveConns` já são públicos no Handler. Podemos criar uma interface `HandlerMetrics` ou expor o Handler diretamente.

**Decisão proposta**: expor `Handler` diretamente — os campos já são públicos e o pacote `observability` é interno.

#### [NEW] [events.go](file:///home/lucas/Projects/n-backup/internal/server/observability/events.go)

Ring buffer lock-free de eventos operacionais:

```go
type EventRing struct {
    buf   []EventEntry
    pos   atomic.Int64
    cap   int
}

func NewEventRing(capacity int) *EventRing
func (r *EventRing) Push(e EventEntry)
func (r *EventRing) Recent(limit int) []EventEntry // cópia snapshot
```

#### [NEW] [http.go](file:///home/lucas/Projects/n-backup/internal/server/observability/http.go)

Router HTTP usando `net/http` puro (sem dependência extra):

```go
func NewRouter(snap *Snapshotter, acl *ACL) http.Handler
```

Rotas:
| Método | Path | Handler |
|--------|------|---------|
| GET | `/api/v1/health` | → `snap.Health()` |
| GET | `/api/v1/metrics` | → `snap.Metrics()` |
| GET | `/api/v1/sessions` | → `snap.Sessions()` |
| GET | `/api/v1/sessions/{id}` | → `snap.Session(id)` |
| GET | `/api/v1/events` | → `snap.events.Recent(limit)` |
| GET | `/api/v1/config/effective` | → `snap.ConfigEffective()` |
| GET | `/` | → SPA (`embed.FS`) |

#### [NEW] [http_test.go](file:///home/lucas/Projects/n-backup/internal/server/observability/http_test.go)

Testes com `httptest.NewServer`:
- Health retorna 200 + JSON válido
- Metrics retorna 200 + JSON válido
- Sessions vazio → array vazio
- Session com ID inexistente → 404
- ACL bloqueia → 403

---

### Server Integration

#### [MODIFY] [server.go](file:///home/lucas/Projects/n-backup/internal/server/server.go)

Na função `Run()`:
1. Após criar o `handler`, verificar `cfg.WebUI.Enabled`
2. Se sim: criar `Snapshotter`, `ACL`, router → iniciar `http.Server` em goroutine
3. Registrar shutdown graceful no context cancel

```diff
+// Web UI HTTP server
+if cfg.WebUI.Enabled {
+    acl, err := observability.NewACL(cfg.WebUI.AllowOrigins)
+    if err != nil {
+        return fmt.Errorf("web UI ACL: %w", err)
+    }
+    events := observability.NewEventRing(1000)
+    snap := observability.NewSnapshotter(handler, cfg, events)
+    router := observability.NewRouter(snap, acl)
+    webSrv := &http.Server{
+        Addr:         cfg.WebUI.Listen,
+        Handler:      router,
+        ReadTimeout:  cfg.WebUI.ReadTimeout,
+        WriteTimeout: cfg.WebUI.WriteTimeout,
+        IdleTimeout:  cfg.WebUI.IdleTimeout,
+    }
+    go func() {
+        logger.Info("web UI listening", "address", cfg.WebUI.Listen)
+        if err := webSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
+            logger.Error("web UI server error", "error", err)
+        }
+    }()
+    go func() {
+        <-ctx.Done()
+        webSrv.Shutdown(context.Background())
+    }()
+}
```

Aplicar o mesmo padrão em `RunWithListener` para testes.

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

Adicionar getter público para `sessions`:

```go
func (h *Handler) Sessions() *sync.Map { return h.sessions }
```

Exportar `Cfg()` se necessário para `ConfigEffective`.

---

### SPA Frontend (Fase 4)

#### [NEW] `web/` directory

Estrutura:
```
web/
  index.html           # Shell SPA
  css/
    style.css          # Design system (dark mode, glassmorphism)
  js/
    app.js             # Router + polling
    api.js             # Fetch wrapper
    components.js      # Render functions
```

- **Overview**: cards com sessões ativas, throughput total, conns, alertas. Mini-gráfico throughput (canvas, últimos 5 min).
- **Sessões**: tabela com filtro, coluna ETA.
- **Detalhe**: streams com offsets, throughput, idle, timeline de eventos.
- **Config**: config efetiva JSON formatada.
- Polling 2s, backoff com `document.visibilityState`.
- Vanilla JS, sem framework, sem build step.

#### [NEW] [embed.go](file:///home/lucas/Projects/n-backup/internal/server/observability/embed.go)

```go
//go:embed web
var webFS embed.FS

// ServeWeb retorna http.Handler que serve os assets da SPA
func ServeWeb() http.Handler
```

---

### Documentação (Fase 5)

#### [MODIFY] [usage.md](file:///home/lucas/Projects/n-backup/docs/usage.md)

Seção nova: **Web UI de Observabilidade** com endpoints, configuração, exemplos de curl.

#### [MODIFY] [installation.md](file:///home/lucas/Projects/n-backup/docs/installation.md)

Nota sobre porta interna + ACL no deploy.

#### [NEW] [spa_architecture.puml](file:///home/lucas/Projects/n-backup/docs/diagrams/spa_architecture.puml)

Diagrama C4 Container do componente web + interação com Handler.

---

## Verificação

### Testes Automatizados

```bash
# Testes unitários — config parse + validação
cd /home/lucas/Projects/n-backup && go test ./internal/config/ -v -run TestLoadServerConfig

# Testes unitários — ACL middleware
cd /home/lucas/Projects/n-backup && go test ./internal/server/observability/ -v -run TestACL

# Testes HTTP — endpoints
cd /home/lucas/Projects/n-backup && go test ./internal/server/observability/ -v -run TestHTTP

# Testes ring buffer
cd /home/lucas/Projects/n-backup && go test ./internal/server/observability/ -v -run TestEventRing

# Race detection no pacote completo
cd /home/lucas/Projects/n-backup && go test -race ./internal/server/observability/

# Testes existentes não devem quebrar
cd /home/lucas/Projects/n-backup && go test ./...
```

### Verificação Manual

1. **Start do server com web_ui habilitado**:
   - Configurar `web_ui.enabled: true` no YAML
   - Iniciar server e verificar log `"web UI listening"`
   - `curl http://127.0.0.1:9848/api/v1/health` → JSON com status ok

2. **ACL funcional**:
   - Configurar `allow_origins: ["127.0.0.1/32"]`
   - `curl` de localhost → 200
   - `curl` de outro IP → 403

3. **SPA renderiza**:
   - Abrir `http://127.0.0.1:9848/` no browser
   - Verificar cards, tabela, polling ativo

4. **Impacto zero no backup**:
   - Executar backup paralelo enquanto SPA está ativa
   - Verificar que throughput não degrada
