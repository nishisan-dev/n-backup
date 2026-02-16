# Mitigação dos Findings do Code Review (2026-02-16)

Plano para corrigir os 8 findings identificados na revisão estática do projeto n-backup.

> [!IMPORTANT]
> Todos os 8 findings foram **validados e confirmados** como presentes no código atual.
> Nenhum teste existente cobre os cenários reportados.

---

## Proposed Changes

### F1 — CRÍTICO: Panic em `handleResume` com `ParallelSession`

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- **Linha ~1066**: trocar `raw.(*PartialSession)` por type assertion segura (`ps, ok := raw.(*PartialSession)`)
- Se o tipo não for `*PartialSession`, retornar `ResumeStatusNotFound` e logar warning

```diff
-session := raw.(*PartialSession)
+session, ok := raw.(*PartialSession)
+if !ok {
+    logger.Warn("resume: session is not a PartialSession (likely ParallelSession)",
+        "session", resume.SessionID)
+    protocol.WriteResumeACK(conn, protocol.ResumeStatusNotFound, 0)
+    return
+}
```

#### [MODIFY] [handler_flow_rotation_test.go](file:///home/lucas/Projects/n-backup/internal/server/handler_flow_rotation_test.go) ou [NEW] handler_resume_test.go

- Teste: registrar `*ParallelSession` com um sessionID, enviar RSME com esse ID → deve retornar `ResumeStatusNotFound` sem panic

---

### F2 — ALTO: Path Traversal em `agentName`/`backupName`

#### [NEW] [sanitize.go](file:///home/lucas/Projects/n-backup/internal/server/sanitize.go)

- Criar função `validatePathComponent(name string) error` que:
  1. Rejeita strings vazias
  2. Rejeita `..`, `/`, `\` e NUL byte
  3. Rejeita nomes que começam com `.`
  4. Limite de 255 caracteres

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- Chamar `validatePathComponent()` para `agentName`, `storageName` e `backupName` logo após a leitura no handshake
- Rejeitar conexão com `StatusReject` se inválido

#### [MODIFY] [storage.go](file:///home/lucas/Projects/n-backup/internal/server/storage.go)

- No `NewAtomicWriter`, após `filepath.Join`, validar que o resultado resolve para dentro de `baseDir` usando `filepath.Rel()` (defesa em profundidade)

#### [NEW] [sanitize_test.go](file:///home/lucas/Projects/n-backup/internal/server/sanitize_test.go)

- Testes para: `../`, `../../etc/passwd`, nomes com `/`, `\0`, nomes vazios, nomes com `.`

---

### F3 — ALTO: Identidade sem vínculo com certificado mTLS

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- Em `handleBackup()` (L~914), após ler `agentName` do protocolo, extrair CN/SAN do cert TLS peer
- Se mTLS está ativo e CN ≠ agentName, rejeitar com `StatusReject` e mensagem descritiva
- Extrair usando `extractAgentName()` já existente (L~878)

```diff
 agentName, err := readUntilNewline(conn)
+// Valida identidade contra certificado TLS
+certName := h.extractAgentName(conn, logger)
+if certName != "" && certName != agentName {
+    logger.Warn("agent identity mismatch",
+        "protocol_agent", agentName, "cert_cn", certName)
+    protocol.WriteACK(conn, protocol.StatusReject,
+        fmt.Sprintf("agent name %q does not match certificate CN %q", agentName, certName), "")
+    return
+}
```

> [!WARNING]
> Esta mudança é **breaking** para cenários onde o CN do certificado difere do `agentName` configurado no agent. Se houver agents em produção com essa configuração, será necessário alinhar os nomes antes do deploy.

---

### F4 — ALTO: Roteamento incorreto de `ControlRotateACK`

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- **Linha ~840**: alterar `return false` para continuar a iteração quando o `RotatePending` do streamIdx **não existir** na sessão encontrada
- Só retornar `false` (parar iteração) quando efetivamente encontrar e sinalizar o `RotatePending`

```diff
-if ackCh, ok := ps.RotatePending.Load(streamIdx); ok {
-    select {
-    case ackCh.(chan struct{}) <- struct{}{}:
-    default:
-    }
-}
-return false // encontrou, não precisa continuar
+if ackCh, ok := ps.RotatePending.Load(streamIdx); ok {
+    select {
+    case ackCh.(chan struct{}) <- struct{}{}:
+    default:
+    }
+    return false // encontrou e sinalizou, pode parar
+}
+return true // esta sessão não tem RotatePending para este idx, continua
```

#### [MODIFY] [handler_flow_rotation_test.go](file:///home/lucas/Projects/n-backup/internal/server/handler_flow_rotation_test.go)

- Teste: 2 sessões paralelas do mesmo agent, `RotatePending` apenas na 2ª → ACK deve ser roteado corretamente

---

### F5 — MÉDIO: `readUntilNewline` sem limite

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

- Renomear para `readUntilNewlineWithLimit(conn net.Conn, maxLen int) (string, error)`
- Adicionar verificação `len(buf) > maxLen` → retorna erro
- Adicionar deadline de leitura no handshake (10s)

```go
const maxFieldLength = 512

func readUntilNewlineWithLimit(conn net.Conn, maxLen int) (string, error) {
    var buf []byte
    oneByte := make([]byte, 1)
    for {
        if _, err := io.ReadFull(conn, oneByte); err != nil {
            return "", err
        }
        if oneByte[0] == '\n' {
            break
        }
        buf = append(buf, oneByte[0])
        if len(buf) > maxLen {
            return "", fmt.Errorf("field exceeds max length %d", maxLen)
        }
    }
    return string(buf), nil
}
```

- Atualizar todos os call sites de `readUntilNewline()` para usar `readUntilNewlineWithLimit(conn, maxFieldLength)`
- Adicionar `conn.SetReadDeadline(time.Now().Add(10*time.Second))` antes do bloco de handshake e resetar após

---

### F6 — MÉDIO: `ChunkAssembler.Cleanup` não fecha `outFile`

#### [MODIFY] [assembler.go](file:///home/lucas/Projects/n-backup/internal/server/assembler.go)

- No `Cleanup()` (L~400-411):
  1. Verificar se `outFile` não é nil e `finalized` é false
  2. Fechar `outFile`
  3. Remover `outPath`

```diff
 func (ca *ChunkAssembler) Cleanup() error {
     ca.mu.Lock()
     defer ca.mu.Unlock()

+    if ca.outFile != nil && !ca.finalized {
+        ca.outFile.Close()
+        ca.outFile = nil
+        os.Remove(ca.outPath)
+    }
+
     if ca.chunkDirExists {
         os.RemoveAll(ca.chunkDir)
     }
```

#### [MODIFY] [assembler_test.go](file:///home/lucas/Projects/n-backup/internal/server/assembler_test.go)

- Teste: Cleanup sem Finalize → `outPath` não deve existir e `outFile` deve estar fechado

---

### F7 — MÉDIO: Data race em `active`/`dead` no dispatcher

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

- Converter `active` e `dead` para `atomic.Bool` em `ParallelStream`
- Atualizar todos os acessos (`stream.active` → `stream.active.Load()`, atribuições para `.Store()`)

```diff
 type ParallelStream struct {
     index      uint8
     rb         *RingBuffer
     conn       net.Conn
     connMu     sync.Mutex
     sendOffset int64
     sendMu     sync.Mutex
     drainBytes int64
-    active     bool
-    dead       bool
+    active     atomic.Bool
+    dead       atomic.Bool
     senderDone chan struct{}
     senderErr  chan error
 }
```

**Locais de atualização** (todos no `dispatcher.go`):
- `NewDispatcher` L~136-137
- `emitChunk` L~204
- `startSenderWithRetry` L~275, 300, 331, 346, 356
- `ActivateStream` L~491, 541
- `DeactivateStream` L~560, 564
- `SampleRates` L~634
- `Close` L~652
- `WaitAllSenders` L~679, 683

---

### F8 — MÉDIO: Data race em `BackupJob.LastResult`

#### [MODIFY] [scheduler.go](file:///home/lucas/Projects/n-backup/internal/agent/scheduler.go)

- Proteger todas as escritas de `LastResult` com `job.mu`
- L~116 (skipped): mover `job.LastResult = ...` para dentro da seção `job.mu.Lock()`
- L~136: idem
- L~163: idem
- L~170: idem

```diff
 if job.running {
     job.mu.Unlock()
     entryLogger.Warn("backup already running, skipping scheduled execution")
-    job.LastResult = &BackupJobResult{...}
+    job.mu.Lock()
+    job.LastResult = &BackupJobResult{...}
+    job.mu.Unlock()
     return
 }
```

> [!NOTE]
> Na L~116, `job.mu.Unlock()` é chamado logo acima. É necessário re-adquirir o lock antes de escrever `LastResult`.

---

## Verification Plan

### Automated Tests

Todos os comandos abaixo devem ser executados a partir da raiz do projeto (`/home/lucas/Projects/n-backup`):

1. **Testes existentes (sanity check)**:
   ```bash
   go test ./...
   ```

2. **Race detector** (valida findings F7 e F8):
   ```bash
   go test -race ./internal/agent ./internal/server
   ```

3. **Testes novos específicos** (serão criados como parte da implementação):
   - `TestHandleResume_ParallelSessionDoesNotPanic` (F1)
   - `TestValidatePathComponent_*` (F2)
   - `TestControlRotateACK_MultipleSessionsSameAgent` (F4)
   - `TestReadUntilNewlineWithLimit_*` (F5)
   - `TestChunkAssembler_CleanupClosesOutFile` (F6)

### Manual Verification

- Para F3 (identidade mTLS): testar em ambiente real com agent cujo `agentName` difere do CN do certificado — o server deve rejeitar a conexão
