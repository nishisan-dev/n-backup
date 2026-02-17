# Melhorias WebUI: Versão + Assembly ETA + Overview

## Contexto

Três melhorias identificadas para o WebUI de observabilidade do n-backup:
1. **Fix versão "dev"** — ldflags injeta `main.version` mas o WebUI lê `observability.Version` que nunca recebe o valor real.
2. **Assembly Progress com Segundo ETA** — após transferência completa, a fase de assembly (especialmente no modo `lazy`) não tem feedback visual nem ETA.
3. **Overview de melhorias** — sugestões pontuais de UX/design.

---

## Proposed Changes

### Componente 1: Fix da Versão

O pipeline de release (`release.yml` L45 e `build-deb.sh` L32) injeta `-X main.version=${VERSION}`, mas `observability.Version` (em `http.go` L21) é declarada como `"dev"` no pacote `observability` — **nunca recebe o valor**. O `main.go` do server nem declara `var version`.

#### [MODIFY] [release.yml](file:///home/lucas/Projects/n-backup/.github/workflows/release.yml)

Alterar ldflags de:
```diff
- LDFLAGS="-s -w -X main.version=${VERSION}"
+ LDFLAGS="-s -w -X github.com/nishisan-dev/n-backup/internal/server/observability.Version=${VERSION}"
```

#### [MODIFY] [build-deb.sh](file:///home/lucas/Projects/n-backup/packaging/build-deb.sh)

Mesma correção:
```diff
- LDFLAGS="-s -w -X main.version=${VERSION}"
+ LDFLAGS="-s -w -X github.com/nishisan-dev/n-backup/internal/server/observability.Version=${VERSION}"
```

> [!NOTE]
> Essa abordagem injeta direto na variável de pacote exportada, eliminando a necessidade de wiring em `main.go`. É o padrão Go idiomático.

---

### Componente 2: Assembly Progress com Segundo ETA

#### Estado Atual
- `AssemblerStats` expõe: `NextExpectedSeq`, `PendingChunks`, `PendingMemBytes`, `TotalBytes`, `Finalized`
- No modo **eager**, chunks são escritos diretamente no arquivo final — assembly é invisível (sem fase separada)
- No modo **lazy**, `finalizeLazy()` percorre chunks de 0 a `lazyMaxSeq` sem contadores intermediários — a UI não tem como mostrar progresso da montagem
- O frontend mostra ETA apenas baseado em objetos transferidos, sem considerar a fase de assembly

#### Mudanças Backend

##### [MODIFY] [dto.go](file:///home/lucas/Projects/n-backup/internal/server/observability/dto.go)

Adicionar campos ao `AssemblerStats`:
```diff
 type AssemblerStats struct {
     NextExpectedSeq uint32 `json:"next_expected_seq"`
     PendingChunks   int    `json:"pending_chunks"`
     PendingMemBytes int64  `json:"pending_mem_bytes"`
     TotalBytes      int64  `json:"total_bytes"`
     Finalized       bool   `json:"finalized"`
+    TotalChunks     uint32 `json:"total_chunks"`
+    AssembledChunks uint32 `json:"assembled_chunks"`
+    Phase           string `json:"phase"` // "receiving" | "assembling" | "done"
 }
```

##### [MODIFY] [assembler.go](file:///home/lucas/Projects/n-backup/internal/server/assembler.go)

1. Adicionar campo `assembledChunks atomic.Uint32` ao `ChunkAssembler`
2. Em `finalizeLazy()`, incrementar `ca.assembledChunks` a cada chunk montado com sucesso
3. Em `Stats()`, retornar `TotalChunks` como `lazyMaxSeq + 1` (modo lazy) ou `nextExpectedSeq` (modo eager) e `AssembledChunks` do atomic
4. Adicionar lógica de `Phase`:
   - `"receiving"` — enquanto chunks estão chegando (`!finalized` e transferência ativa)
   - `"assembling"` — quando `finalizeLazy` está em execução (modo lazy) 
   - `"done"` — quando `finalized == true`

Para identificar a fase `"assembling"` sem race conditions, adicionar um `assembling atomic.Bool` ao ChunkAssembler, setado no início de `finalizeLazy()`.

##### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

Nos métodos `SessionsSnapshot()` e `SessionDetail()`:
- Popular os novos campos `TotalChunks`, `AssembledChunks` e `Phase` a partir de `s.Assembler.Stats()`
- Adicionar novo campo `AssemblyETA` ao `SessionSummary` para o ETA da fase de assembly (calculado a partir de `AssembledChunks/TotalChunks` e velocidade de disco)

##### [MODIFY] [dto.go](file:///home/lucas/Projects/n-backup/internal/server/observability/dto.go) (SessionSummary)

Adicionar campo:
```diff
 ETA          string          `json:"eta,omitempty"`
+AssemblyETA  string          `json:"assembly_eta,omitempty"`
```

#### Mudanças Frontend

##### [MODIFY] [components.js](file:///home/lucas/Projects/n-backup/internal/server/observability/web/js/components.js)

Em `renderSessionDetail()`:
1. Detectar fase de assembly via `detail.assembler.phase`
2. Quando `phase === "assembling"`:
   - Mostrar barra de progresso secundária (assembly) com cor diferente (verde/emerald)
   - Exibir `AssembledChunks / TotalChunks` e o `assembly_eta`
   - Mudar status badge para `assembling`
3. Quando `phase === "done"`: badge `✓ Finalizado`
4. Na lista de sessões (`renderSessionsList`), incluir indicação visual quando sessão estiver em fase de assembly

---

### Componente 3: Overview de Melhorias WebUI

Sugestões que não entram neste PR, mas ficam documentadas:

| Melhoria | Impacto | Complexidade |
|---|---|---|
| Dark mode toggle persistente | UX | Baixa |
| Notificação sonora/visual ao finalizar backup | UX | Média |
| Histórico de sessões finalizadas (últimas N) | Info | Alta |
| Gráfico de throughput agregado no overview | Viz | Média |
| Tempo total de sessão no card | Info | Baixa |
| Compressão ratio (bytes_received vs disk_write) | Info | Baixa |
| Auto-refresh rate selector na UI | UX | Baixa |

> [!TIP]
> Dessas melhorias, as de **baixa complexidade/alto impacto** (compression ratio, tempo total, auto-refresh selector) podem ser feitas neste mesmo PR se desejado.

---

## Verification Plan

### Testes Automatizados

```bash
# Testes existentes que validam backend de observabilidade + assembler
go test ./internal/server/... -count=1 -v -timeout 120s
```

Testes cobertos:
- `assembler_test.go` — 7 testes validando in-order, out-of-order, lazy mode, cleanup
- `http_test.go` — 8 testes validando health, metrics, sessions, agents, ACL

Os novos campos `TotalChunks`, `AssembledChunks` e `Phase` são aditivos (novos campos JSON), logo **não quebram testes existentes**. Será adicionado um novo teste em `assembler_test.go` para validar o campo `AssembledChunks` incrementa durante `finalizeLazy()`.

### Validação Visual

Após implementação, será feito um build local e validação visual da WebUI via browser usando a ferramenta de browser agent para verificar:
1. Versão exibida corretamente (não "dev") após build com ldflags
2. Barra de progresso de assembly aparece na view de detalhe
3. ETA de assembly calculado e exibido corretamente
