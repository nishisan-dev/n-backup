# Melhorias no Progress Reporter do Agent

O `ProgressReporter` atual tem dois problemas:

1. **ETA incorreto** — Calculado apenas com base em bytes transferidos vs velocidade. Quando a transferência em tamanho já concluiu (ex: compressão agressiva), mas objetos ainda restam, o ETA mostra `0:0000` em vez de estimar o tempo restante baseado na taxa de objetos.
2. **Sem visibilidade de streams** — Em modo paralelo (`maxStreams > 1`), não há indicação de quantos streams estão ativos vs o máximo configurado.

## Proposed Changes

### Progress Reporter

#### [MODIFY] [progress.go](file:///home/lucas/Projects/n-backup/internal/agent/progress.go)

**1. ETA Pessimista (dual-metric)**

Calcular dois ETAs independentes e usar o **mais pessimista** (maior):

- **ETA por bytes**: `(totalBytes - bytesWritten) / bytesPerSec` (lógica atual)
- **ETA por objetos**: `(totalObjects - objectsDone) / objectsPerSec`

O ETA final será `max(etaBytes, etaObjects)`. Quando um dos eixos já completou (remaining ≤ 0), o ETA vem exclusivamente do outro eixo.

**2. Exibição de streams ativos**

Adicionar campos atômicos ao `ProgressReporter`:
- `activeStreams atomic.Int32` — streams ativos no momento
- `maxStreams atomic.Int32` — máximo configurado

Novo método `SetStreams(active, max int)` chamado pelo pipeline.

Na renderização, quando `maxStreams > 1`, exibir `⇅ 3/4` (active/max) na barra de progresso.

---

### Integração no Pipeline

#### [MODIFY] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

Na função `runParallelBackup`, conectar o `ProgressReporter` ao `Dispatcher` para reportar streams:
- Após criar o dispatcher, chamar `progress.SetStreams(dispatcher.ActiveStreams(), entry.Parallels)` 
- Modificar `ActivateStream` e `DeactivateStream` no dispatcher para invocar um callback que atualize o progress

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

Adicionar campo `onStreamChange func(active, max int)` ao `Dispatcher` e `DispatcherConfig`. 
Invocar o callback em `ActivateStream` e `DeactivateStream` para notificar mudanças.

---

### Testes

#### [NEW] [progress_test.go](file:///home/lucas/Projects/n-backup/internal/agent/progress_test.go)

Testes unitários para as novas lógicas:
- `TestETA_PessimisticUsesObjects` — quando bytes restantes = 0 mas objetos restam, ETA vem dos objetos
- `TestETA_PessimisticUsesBytes` — quando objetos restam e bytes restam, ETA = max(etaBytes, etaObjects)
- `TestETA_BothComplete` — quando ambos completaram, ETA = `0:00`
- `TestStreamsDisplay` — verifica que `SetStreams` e a renderização produzem o formato `⇅ x/n`

## Verification Plan

### Automated Tests

```bash
cd /home/lucas/Projects/n-backup && go test ./internal/agent/ -run TestETA -v
cd /home/lucas/Projects/n-backup && go test ./internal/agent/ -run TestStreamsDisplay -v
```

### Manual Verification

> [!IMPORTANT]
> A melhor forma de validar visualmente é o usuário executar um backup real com `--once --progress` e observar que:
> 1. O ETA não chega a `0:0000` enquanto objetos ainda estão sendo transferidos
> 2. Em modo paralelo, a contagem de streams `⇅ x/n` aparece e reflete ativação/desativação
