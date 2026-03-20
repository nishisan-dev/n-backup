# Plano Evolutivo — Resiliência de Sessão Paralela

Decorrente do [post-mortem de 20/03/2026](file:///home/lucas/Projects/n-backup/planning/postmortem_session_lost_2026-03-20.md), onde ~38 GB foram transferidos mas **nunca commitados** porque o control channel caiu e a sessão ficou orphaned até ser removida pelo cleanup sem registro.

O plano é dividido em **3 fases incrementais**, cada uma entregando valor independente e podendo ser commitada/testada isoladamente.

---

## Fase 1 — Registro de Sessões Expiradas (Quick Win)

**Objetivo**: Garantir que sessões removidas pelo `CleanupExpiredSessions` sejam registradas no histórico e gerem evento de alerta. Resolve a invisibilidade total ("sem visibilidade no dashboard").

### Motivação

Hoje o `CleanupExpiredSessions` em [handler_storage.go](file:///home/lucas/Projects/n-backup/internal/server/handler_storage.go#L131-L177) faz `sessions.Delete(key)` sem chamar `recordSessionEnd()` e sem emitir evento. A sessão desaparece silenciosamente.

### Alterações

#### [MODIFY] [handler_storage.go](file:///home/lucas/Projects/n-backup/internal/server/handler_storage.go)

A função `CleanupExpiredSessions` precisa receber um ponteiro para o `Handler` (ou as dependências necessárias) para poder chamar `recordSessionEnd` e `Events.PushEvent`.

**Opção escolhida**: Transformar `CleanupExpiredSessions` de função free em **método** do `Handler`. Isso dá acesso direto a `h.SessionHistory`, `h.Events` e `h.recordSessionEnd()`.

```diff
-func CleanupExpiredSessions(sessions *sync.Map, ttl time.Duration, logger *slog.Logger) {
+func (h *Handler) CleanupExpiredSessions(ttl time.Duration, logger *slog.Logger) {
+    sessions := h.sessions
```

Dentro do `case *PartialSession:`, antes de `sessions.Delete(key)`:
```diff
+    h.recordSessionEnd(key.(string), s.AgentName, s.StorageName, s.BackupName, "single", s.CompressionMode, "expired", s.CreatedAt, s.BytesWritten.Load())
+    if h.Events != nil {
+        h.Events.PushEvent("error", "session_expired", s.AgentName, fmt.Sprintf("%s/%s expired (idle %s)", s.StorageName, s.BackupName, time.Since(lastAct).Round(time.Second)), 0)
+    }
```

Dentro do `case *ParallelSession:`, antes de `sessions.Delete(key)`:
```diff
+    h.recordSessionEnd(key.(string), s.AgentName, s.StorageName, s.BackupName, "parallel", s.StorageInfo.CompressionMode, "expired", s.CreatedAt, s.DiskWriteBytes.Load())
+    if h.Events != nil {
+        h.Events.PushEvent("error", "session_expired", s.AgentName, fmt.Sprintf("%s/%s expired (idle %s)", s.StorageName, s.BackupName, time.Since(lastAct).Round(time.Second)), 0)
+    }
```

#### [MODIFY] Callers de `CleanupExpiredSessions`

Precisamos atualizar todos os callers para usar a nova assinatura como método do Handler:

```diff
-CleanupExpiredSessions(sessions, ttl, logger)
+h.CleanupExpiredSessions(ttl, logger)
```

> [!IMPORTANT]
> Precisamos localizar os callers via `grep` e atualizar. O principal deve estar em `server.go` no loop de background.

#### [MODIFY] [server_test.go](file:///home/lucas/Projects/n-backup/internal/server/server_test.go)

Os 3 testes existentes (`TestCleanupExpiredSessions_MixedTypes`, `_ParallelSessionClosesStreamsAndCancels`, `_ActiveSessionNotCleaned`) precisam ser adaptados para a nova assinatura. Criaremos um `Handler` mínimo com `sessions` e `SessionHistory` para validar.

Adicionar novo teste:

- **`TestCleanupExpiredSessions_RecordsExpiredInHistory`**: verifica que uma sessão expirada gera entrada no `SessionHistory` com `result: "expired"` e que um evento `session_expired` é emitido.

---

## Fase 2 — Monitoramento do Control Channel na Sessão Paralela

**Objetivo**: Quando o control channel cai durante uma sessão ativa, a sessão deve ser **abortada controladamente** em vez de ficar presa indefinidamente no select esperando `IngestionDone`.

### Motivação

O select bloqueante em [handler_parallel.go:225](file:///home/lucas/Projects/n-backup/internal/server/handler_parallel.go#L225-L264) espera `IngestionDone`, `Aborted`, timeout de 25h ou `ctx.Done()`. Quando o control channel cai por EOF, o `ControlIngestionDone` nunca chega, e o `ctx.Done()` não dispara se a conn primária não tem listener ativo. A sessão fica presa até o timeout de 25h ou o cleanup de 1h.

### Alterações

#### [MODIFY] [handler_parallel.go](file:///home/lucas/Projects/n-backup/internal/server/handler_parallel.go)

Adicionar um campo `ControlLost chan struct{}` ao `ParallelSession`:

```diff
 type ParallelSession struct {
     // ... campos existentes ...
+    ControlLost     chan struct{} // fechado quando o control channel deste agent cai
+    controlLostOnce sync.Once
 }
```

No construtor da sessão em `handleParallelBackup` (~L173):

```diff
     pSession := &ParallelSession{
         // ... campos existentes ...
+        ControlLost: make(chan struct{}),
     }
```

No select bloqueante (~L225), adicionar `ControlLost`:

```diff
 select {
 case <-pSession.IngestionDone:
     logger.Info("agent confirmed ingestion complete")
+case <-pSession.ControlLost:
+    // Control channel caiu — espera um tempo para reconexão antes de abortar
+    logger.Warn("control channel lost during active session, waiting for reconnection")
+    select {
+    case <-pSession.IngestionDone:
+        logger.Info("agent delivered IngestionDone after control reconnect")
+    case <-time.After(5 * time.Minute):
+        logger.Error("control channel not recovered after grace period — aborting session")
+        pSession.abort(fmt.Errorf("control channel lost and not recovered within 5m"))
+        h.sessions.Delete(sessionID)
+        h.recordSessionEnd(sessionID, agentName, storageName, backupName, "parallel", storageInfo.CompressionMode, "control_lost", now, pSession.DiskWriteBytes.Load())
+        if h.Events != nil {
+            h.Events.PushEvent("error", "session_control_lost", agentName, fmt.Sprintf("%s/%s aborted: control channel lost", storageName, backupName), 0)
+        }
+        protocol.WriteFinalACK(conn, protocol.FinalStatusWriteError)
+        return
+    }
 case <-pSession.Aborted:
     // ... existente ...
```

#### [MODIFY] [handler_control.go](file:///home/lucas/Projects/n-backup/internal/server/handler_control.go)

Quando o control channel fecha (saída do loop for), sinalizar `ControlLost` em todas as sessões ativas deste agent:

```diff
 // Antes do return no final do handleControlChannel (após sair do for loop):
+    // Sinaliza perda do control channel para sessões ativas deste agent
+    h.sessions.Range(func(_, value any) bool {
+        ps, ok := value.(*ParallelSession)
+        if !ok || ps.AgentName != agentName {
+            return true
+        }
+        ps.controlLostOnce.Do(func() { close(ps.ControlLost) })
+        return true
+    })
```

> [!WARNING]
> Quando o agent reconecta o control channel, o novo `handleControlChannel` precisa **resetar** o `ControlLost` da sessão. Porém, channels em Go não podem ser "reabertos". A solução é: quando o `ControlLost` já estiver fechado e o agent reconectar (novo `handleControlChannel` com mesmo `agentName`), criar um **novo channel** e fazer swap atômico. Isso é tratado na Fase 3.

---

## Fase 3 — Reassociação do Control Channel

**Objetivo**: Quando o agent reconecta o control channel, a sessão existente recebe o novo contexto e pode continuar o fluxo normalmente (inclusive receber o `ControlIngestionDone`).

### Motivação

Hoje o `handleControlChannel` já faz lookup direto por `sessionID` para `ControlIngestionDone` (L307). Porém, quando o agent reconecta, os frames enviados na nova conexão vão para uma nova goroutine `handleControlChannel` que **não tem relação direta** com a sessão. O `ControlIngestionDone` funciona por sessionID, então na reconexão ele deveria funcionar — **o problema real é que a goroutine de `handleParallelBackup` já saiu** porque `ctx.Done()` disparou quando a conn primária fechou.

Com a Fase 2, a goroutine de handshake não sai mais por `ctx.Done()` da conn primária, mas fica no grace period de `ControlLost`. Agora precisamos que a reconexão **cancele o ControlLost**.

### Alterações

#### [MODIFY] [handler_parallel.go](file:///home/lucas/Projects/n-backup/internal/server/handler_parallel.go)

Adicionar método para reset do `ControlLost`:

```go
// resetControlLost recria o channel ControlLost para uma reconexão do control channel.
// Deve ser chamado APENAS quando o control channel foi restabelecido para este agent.
func (ps *ParallelSession) resetControlLost() {
    ps.controlLostOnce = sync.Once{}
    ps.ControlLost = make(chan struct{})
}
```

> [!CAUTION]
> O reset do `sync.Once` + channel precisa ser thread-safe. A goroutine no grace period está lendo `ps.ControlLost`. Usaremos um `sync.Mutex` dedicado para proteger a troca.

Refinamento com mutex:

```go
type ParallelSession struct {
    // ...
    ControlLost     chan struct{}
    controlLostMu   sync.Mutex
    controlLostOnce sync.Once
}

func (ps *ParallelSession) signalControlLost() {
    ps.controlLostMu.Lock()
    defer ps.controlLostMu.Unlock()
    ps.controlLostOnce.Do(func() { close(ps.ControlLost) })
}

func (ps *ParallelSession) resetControlLost() chan struct{} {
    ps.controlLostMu.Lock()
    defer ps.controlLostMu.Unlock()
    ch := make(chan struct{})
    ps.ControlLost = ch
    ps.controlLostOnce = sync.Once{}
    return ch
}
```

#### [MODIFY] [handler_control.go](file:///home/lucas/Projects/n-backup/internal/server/handler_control.go)

No **início** do `handleControlChannel`, após registrar a conn, verificar se existem sessões do agent com `ControlLost` fechado e resetar:

```go
// Reassociação: se há sessão ativa deste agent com ControlLost sinalizado,
// a reconexão do control channel cancela o grace period.
h.sessions.Range(func(_, value any) bool {
    ps, ok := value.(*ParallelSession)
    if !ok || ps.AgentName != agentName {
        return true
    }
    select {
    case <-ps.ControlLost:
        // Control channel estava perdido — reconexão restabelece
        ps.resetControlLost()
        logger.Info("control channel reassociated with active session", "session", ps.SessionID)
    default:
        // Control channel não estava perdido — nada a fazer
    }
    return true
})
```

No select do grace period (Fase 2), monitorar o **novo** channel:

```diff
 case <-pSession.ControlLost:
     logger.Warn("control channel lost during active session, waiting for reconnection")
+    graceCh := pSession.ControlLost // snapshot do channel atual
     select {
     case <-pSession.IngestionDone:
         logger.Info("agent delivered IngestionDone after control reconnect")
-    case <-time.After(5 * time.Minute):
+    case <-time.After(5 * time.Minute):
         // ... abortar ...
     }
```

> [!IMPORTANT]
> A lógica de grace period precisa ser refinada: quando `resetControlLost()` é chamado, o channel antigo (que está no select) **já está fechado** e não voltará a bloquear. A reconexão se manifesta pelo `IngestionDone` ser fechado durante o grace period.

---

## Documentação e Diagramas

> [!IMPORTANT]
> Cada fase deve incluir atualização de documentação como parte do commit. A documentação não é pós-deliverable — é parte do deliverable.

### Fase 1 — Documentação

#### [MODIFY] [architecture.md](file:///home/lucas/Projects/n-backup/docs/architecture.md)

- **Seção 8 (Observabilidade)**: adicionar resultado `expired` na lista de badges do Session History e o novo evento `session_expired` na documentação de eventos
- **Tabela de Componentes do server** (seção 3.2): atualizar descrição do `HandlerStorage` para mencionar registro de sessões expiradas no histórico

#### [MODIFY] [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md)

- **Seção 5.4** (Resume de Backups): adicionar nota sobre sessões expiradas serem registradas no histórico com resultado `expired` (antes eram deletadas silenciosamente)

#### [MODIFY] [CHANGELOG.md](file:///home/lucas/Projects/n-backup/CHANGELOG.md)

- Entrada na versão corrente com a correção: "Sessões expiradas agora são registradas no histórico com resultado `expired` e emitem evento `session_expired`"

### Fase 2 — Documentação

#### [MODIFY] [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md)

- **Seção 5.7** (Control Channel): adicionar subseção documentando o comportamento de detecção de queda do control channel durante sessão ativa, grace period configurável, e resultado `control_lost`
- Nova subseção **5.8 — Monitoramento de Control Channel na Sessão Paralela**

#### [MODIFY] [architecture.md](file:///home/lucas/Projects/n-backup/docs/architecture.md)

- **Seção 7** (Resiliência → Control Channel): documentar o novo campo `ControlLost` e o grace period
- **Seção 8** (Observabilidade): adicionar evento `session_control_lost` e resultado `control_lost` no Session History

#### [NEW] [control_channel_resilience.puml](file:///home/lucas/Projects/n-backup/docs/diagrams/control_channel_resilience.puml)

Diagrama de sequência PlantUML mostrando:
1. Sessão paralela ativa com control channel
2. Queda do control channel (EOF)
3. Sinalização de `ControlLost` para a sessão
4. Grace period de 5 minutos
5. Reconexão do agent (Fase 3) ou abort após expiração

### Fase 3 — Documentação

#### [MODIFY] [specification.md](file:///home/lucas/Projects/n-backup/docs/specification.md)

- **Nova seção 5.8** (ou extensão da seção criada na Fase 2): documentar mecanismo de reassociação do control channel, `resetControlLost()`, e comportamento de recuperação

#### [MODIFY] [architecture.md](file:///home/lucas/Projects/n-backup/docs/architecture.md)

- **Seção 7** (Resiliência → Control Channel): documentar o fluxo completo de queda + reconexão + reassociação

#### [MODIFY] [control_channel_resilience.puml](file:///home/lucas/Projects/n-backup/docs/diagrams/control_channel_resilience.puml)

- Adicionar fluxo alternativo de reconexão bem-sucedida durante o grace period

### Definition of Done — Documentação (por fase)

- [ ] Markdown atualizado em `docs/` reflete o comportamento implementado
- [ ] Diagrama PlantUML em `docs/diagrams/` criado/atualizado (quando aplicável)
- [ ] Markdown incorpora diagramas via `uml.nishisan.dev`
- [ ] CHANGELOG.md atualizado com entrada descritiva
- [ ] Conteúdo documentado coerente com o código atual

---

## Verificação

### Testes Automatizados

**Comando para executar todos os testes do pacote server:**
```bash
cd /home/lucas/Projects/n-backup && go test ./internal/server/... -v -count=1 -timeout 120s
```

**Fase 1 — Testes existentes adaptados:**
- `TestCleanupExpiredSessions_MixedTypes` → adaptar para nova assinatura
- `TestCleanupExpiredSessions_ParallelSessionClosesStreamsAndCancels` → adaptar
- `TestCleanupExpiredSessions_ActiveSessionNotCleaned` → adaptar
- **Novo**: `TestCleanupExpiredSessions_RecordsExpiredInHistory`

**Fase 2 — Novos testes:**
- `TestParallelSession_ControlLost_AbortsAfterGrace`: cria sessão, sinaliza `ControlLost`, verifica que após grace period a sessão é abortada
- `TestHandleControlChannel_SignalsControlLost`: simula saída do loop e verifica que sessões ativas recebem sinal

**Fase 3 — Novos testes:**
- `TestParallelSession_ControlLost_ResetOnReconnect`: cria sessão, sinaliza `ControlLost`, chama `resetControlLost()`, verifica que novo channel está aberto
- `TestHandleControlChannel_ReassociatesActiveSession`: simula reconexão e verifica reset do `ControlLost`

### Verificação Manual

Verificação via dashboard do NBackup (WebUI) — acessível em [nbackup.home.nishisan.dev](https://nbackup.home.nishisan.dev):

1. **Fase 1**: Verificar que sessões expiradas aparecem no histórico com resultado `expired` e que o evento `session_expired` aparece na aba Events do dashboard
2. **Fase 2/3**: Requere simulação de queda de control channel durante backup ativo — mais prático de validar via testes unitários

---

## Ordem de Implementação

| Fase | Risco | Complexidade | Valor Imediato |
|------|-------|-------------|----------------|
| **1** | Baixo | Baixa | **Alto** — visibilidade de falhas |
| **2** | Médio | Média | **Alto** — sessões não ficam orphaned |
| **3** | Médio-Alto | Alta | **Médio** — sessão pode recuperar de queda transitória |

> [!TIP]
> Recomendo implementar e commitar uma fase por vez. A Fase 1 pode ser deployada imediatamente e já resolve o problema de invisibilidade.
