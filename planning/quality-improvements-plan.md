# Quality Improvements — Refatoração, Testes e CI

Branch: `refactor/quality-improvements`

Três itens puramente de engenharia que aumentam a manutenibilidade e confiabilidade sem alterar comportamento.

---

## Item 1: Refatoração do `handler.go` (2.664 linhas → ~7 arquivos)

Divisão puramente organizacional — **zero mudanças de API, assinatura ou lógica**. Cada função é movida integralmente para seu novo arquivo. O `handler.go` mantém structs, constructors e o dispatch central.

### Mapeamento de Funções por Novo Arquivo

#### [NEW] [handler_single.go](file:///home/lucas/Projects/n-backup/internal/server/handler_single.go)
Backup single-stream e resume:

| Função | Linhas atuais |
|--------|---:|
| `handleBackup` | 1416–1609 |
| `handleResume` | 1611–1709 |
| `receiveWithSACK` | 1711–1768 |
| `validateAndCommit` | 1770–1848 |
| `sendACK` | 1918–1926 |
| `readUntilNewline` | 1928–1946 |
| `readTrailerFromFile` | 1948–1967 |
| `hashFile` | 1969–1987 |
| `generateSessionID` | 1989–1997 |

---

#### [NEW] [handler_parallel.go](file:///home/lucas/Projects/n-backup/internal/server/handler_parallel.go)
Backup paralelo, join e assembly:

| Função | Linhas atuais |
|--------|---:|
| `ParallelSession` (struct + métodos) | 2048–2112 |
| `handleParallelBackup` | 2114–2345 |
| `readParallelChunkPayload` | 2347–2365 |
| `receiveParallelStream` | 2367–2493 |
| `handleParallelJoin` | 2495–2663 |
| `validateAndCommitWithTrailer` | 1850–1912 |

---

#### [NEW] [handler_control.go](file:///home/lucas/Projects/n-backup/internal/server/handler_control.go)
Control channel:

| Função | Linhas atuais |
|--------|---:|
| `ControlConnInfo` (struct) | 104–111 |
| `registerControlConn` | 113–117 |
| `unregisterControlConn` | 119–125 |
| `handleControlChannel` | 1068–1394 |
| `evaluateFlowRotation` | 865–938 |
| `rotateStream` | 940–1023 |

---

#### [NEW] [handler_health.go](file:///home/lucas/Projects/n-backup/internal/server/handler_health.go)
Health check — arquivo simples:

| Função | Linhas atuais |
|--------|---:|
| `handleHealthCheck` | 1056–1066 |

---

#### [NEW] [handler_observability.go](file:///home/lucas/Projects/n-backup/internal/server/handler_observability.go)
Metrics, snapshots, stats e WebUI-related:

| Função | Linhas atuais |
|--------|---:|
| `MetricsSnapshot` | 147–162 |
| `ChunkBufferStats` | 164–184 |
| `ConnectedAgents` | 186–239 |
| `SessionHistorySnapshot` | 371–377 |
| `ActiveSessionHistorySnapshot` | 379–385 |
| `SessionsSnapshot` | 387–507 |
| `SessionDetail` | 509–695 |
| `formatBytesGo` | 697–709 |
| `sessionStatus` | 711–722 |
| `streamStatus` | 724–747 |
| `streamStat` (struct) | 806–810 |
| `StartStatsReporter` | 749–803 |
| `logPerStreamStats` | 812–859 |
| `recordSessionEnd` | 339–369 |

---

#### [NEW] [handler_storage.go](file:///home/lucas/Projects/n-backup/internal/server/handler_storage.go)
Storage scan, usage e cleanup:

| Função | Linhas atuais |
|--------|---:|
| `StorageUsageSnapshot` | 241–251 |
| `refreshStorageCache` | 253–256 |
| `StartStorageScanner` | 258–275 |
| `scanStorages` | 277–316 |
| `countBackups` | 318–337 |
| `CleanupExpiredSessions` | 1999–2045 |

---

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)
Permanece com:

| Item | Linhas atuais |
|------|---:|
| Constantes (`readInactivityTimeout`, etc.) | 1–54 |
| `PartialSession` (struct) | 55–66 |
| `Handler` (struct) | 69–101 |
| `NewHandler` | 127–136 |
| `StartChunkBuffer` | 138–145 |
| `HandleConnection` (dispatch) | 1025–1054 |
| `extractAgentName` | 1396–1414 |
| `maxHandshakeFieldLen` | 1913–1917 |

> **Estimativa**: `handler.go` reduz de 2.664 → ~300 linhas.

---

## Item 2: Cobertura de Testes

Foco nos dois arquivos de maior criticidade que não têm testes dedicados.

### [NEW] [autoscaler_test.go](file:///home/lucas/Projects/n-backup/internal/agent/autoscaler_test.go)

> [!NOTE]
> Já existem 5 testes de AutoScaler em `dispatcher_test.go` (Hysteresis, Disabled, Adaptive Probe Success/Fail, ScaleDown Cooldown, SkipsInactiveHole). O novo arquivo conterá testes **complementares** focados em cenários não cobertos.

Testes a criar:
- `TestAutoScaler_EfficiencyMode_ScaleUp` — eficiência baixa causa scale-up
- `TestAutoScaler_EfficiencyMode_ScaleDown` — eficiência alta causa scale-down
- `TestAutoScaler_EfficiencyMode_Stable` — eficiência na faixa estável não altera
- `TestAutoScaler_Adaptive_MaxStreamsNoop` — probe não tenta quando já está no máximo
- `TestAutoScaler_Snapshot_ThreadSafe` — leitura concorrente de snapshot não race

### [NEW] [backup_test.go](file:///home/lucas/Projects/n-backup/internal/agent/backup_test.go)

Testes unitários para funções auxiliares **testáveis sem rede**. Foco no `teeWriter` e helpers.

Testes a criar:
- `TestTeeWriter_WritesToBoth` — escrita duplicada em dois destinos
- `TestTeeWriter_PropagatesError` — erro no destino A propaga
- `TestDialWithContext_CancelledContext` — contexto cancelado retorna erro imediato

---

## Item 3: CI Robusto

### [MODIFY] [ci.yml](file:///home/lucas/Projects/n-backup/.github/workflows/ci.yml)

Mudanças:

```diff
 jobs:
   test:
     name: Test
     runs-on: ubuntu-latest

     steps:
       - uses: actions/checkout@v4

       - uses: actions/setup-go@v5
         with:
           go-version-file: go.mod

       - name: Vet
         run: go vet ./...

+      - name: Lint
+        uses: golangci/golangci-lint-action@v6
+        with:
+          version: latest

       - name: Test
-        run: go test ./... -v -count=1 -timeout 120s
+        run: go test ./... -v -count=1 -race -timeout 180s
+
+      - name: Coverage
+        run: |
+          go test ./... -coverprofile=coverage.out -covermode=atomic -timeout 180s
+          COVERAGE=$(go tool cover -func=coverage.out | grep total | awk '{print $3}' | tr -d '%')
+          echo "Total coverage: ${COVERAGE}%"
+          if [ "$(echo "$COVERAGE < 30" | bc -l)" -eq 1 ]; then
+            echo "::error::Coverage ${COVERAGE}% is below 30% threshold"
+            exit 1
+          fi
```

Mudanças chave:
1. **`-race`** no step de Test — detecta race conditions
2. **`golangci-lint`** — análise estática (errcheck, govet, staticcheck, ineffassign, etc.)
3. **Coverage threshold a 30%** — baixo para ser realista com o estado atual, pode ser aumentado gradualmente. O timeout sobe para 180s por causa do `-race`.

---

## Plano de Verificação

### Testes Automatizados

Todos os comandos rodam na raiz do projeto `/home/lucas/Projects/n-backup`:

```bash
# 1. Build — confirma que nenhum import quebrou
go build ./...

# 2. Vet — análise estática
go vet ./...

# 3. Testes com race detector — confirma que nada quebrou + novos testes passam
go test ./... -v -count=1 -race -timeout 180s

# 4. Cobertura — verificar que está acima do threshold
go test ./... -coverprofile=coverage.out -covermode=atomic -timeout 180s
go tool cover -func=coverage.out | grep total
```

### Verificação Manual

1. Confirmar que `handler.go` ficou com ~300 linhas (apenas structs, constructor e dispatch)
2. Confirmar que cada novo `handler_*.go` pertence ao `package server` e compila
3. Revisar o CI rodando `go vet`, `golangci-lint`, testes com `-race` e cobertura

### DoD

- [x] Build OK (`go build ./...`)
- [x] Vet OK (`go vet ./...`)
- [x] Testes com `-race` passando sem falhas
- [x] Cobertura total ≥ 30%
- [x] `handler.go` ≤ 350 linhas
- [x] CI atualizado com lint + race + coverage
