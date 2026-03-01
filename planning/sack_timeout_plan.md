# SACK Timeout — Detecção de Conexão Morta por Stream

## Problema

O agent não detecta conexões de dados "penduradas" (TCP alive mas sem progresso). Se o server para de enviar ChunkSACKs sem fechar a conn, o agent fica bloqueado indefinidamente no backpressure do ring buffer.

## Solução

Monitorar o tempo do último ChunkSACK recebido por stream. Se ultrapassar `max(controlChannelRTT × 3, sackTimeoutMin)`, fechar a conn → o sender detecta o erro e entra no fluxo de retry/reconnect já existente.

---

## Proposed Changes

### Dispatcher

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

**1. Nova constante:**

```go
// sackTimeoutMin é o timeout mínimo entre ChunkSACKs antes de considerar a conn morta.
// O timeout efetivo é max(controlChannelRTT × 3, sackTimeoutMin).
sackTimeoutMin = 5 * time.Second

// sackCheckInterval é o intervalo de verificação do SACK timeout no sender.
sackCheckInterval = 1 * time.Second
```

**2. Novo campo em `ParallelStream`:**

```go
lastSACKAt    atomic.Int64 // unix nano do último ChunkSACK recebido (0 = nenhum SACK ainda)
```

**3. Novo campo em `Dispatcher`:**

```go
sackTimeoutFn func() time.Duration // retorna o timeout efetivo (injeta RTT externo)
```

**4. Novo campo em `DispatcherConfig`:**

```go
SACKTimeoutFn func() time.Duration // fornece timeout dinâmico (ex: max(rtt*3, 5s))
```

**5. Atualizar `startACKReader`** — setar `lastSACKAt` a cada SACK recebido:

```go
stream.lastSACKAt.Store(time.Now().UnixNano())
```

**6. Atualizar `startSenderWithRetry`** — após write bem-sucedido, checar SACK timeout:

Se o stream enviou pelo menos 1 chunk e `lastSACKAt` está stale por mais de `sackTimeoutFn()`, **fechar a conn**. O próximo `writeFrame` falhará → entra no retry loop existente.

```go
if stream.lastSACKAt.Load() > 0 {
    elapsed := time.Since(time.Unix(0, stream.lastSACKAt.Load()))
    if elapsed > d.sackTimeoutFn() {
        d.logger.Warn("SACK timeout, closing connection",
            "stream", streamIdx, "elapsed", elapsed)
        stream.connMu.Lock()
        stream.conn.Close()
        stream.connMu.Unlock()
        continue // sender loop detectará o erro no próximo writeFrame
    }
}
```

**7. Atualizar `reconnectStream`** — resetar `lastSACKAt` após reconexão bem-sucedida:

```go
stream.lastSACKAt.Store(time.Now().UnixNano()) // reset na reconexão
```

---

### Agent Backup

#### [MODIFY] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

Fornecer o `SACKTimeoutFn` ao `DispatcherConfig` usando o RTT do control channel:

```go
SACKTimeoutFn: func() time.Duration {
    rtt := controlCh.RTT()       // EWMA RTT do control channel
    timeout := rtt * 3
    if timeout < 5*time.Second {
        timeout = 5 * time.Second
    }
    return timeout
},
```

---

### Testes

#### [MODIFY] [dispatcher_test.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher_test.go)

Novo teste `TestDispatcher_SACKTimeout_ClosesConnection`:
- Cria dispatcher com `SACKTimeoutFn` retornando 100ms
- Envia chunks, **não** envia SACKs de volta
- Verifica que a conn é fechada após o timeout
- Verifica que o sender entra em retry

---

## Verification Plan

### Automated Tests
```bash
go build ./cmd/...
go test -race -count=1 ./internal/agent/...
go test -race -count=1 ./...
```

### Manual
- Executar backup paralelo e observar logs de SACK timeout não aparecerem (operação normal)
- Simular conn morta (ex: iptables DROP) e validar que o timeout é detectado e o stream reconecta
