# Post-Mortem: Sessão de Backup Perdida — 20/03/2026

## Resumo

Uma sessão de backup em modo paralelo **falhou silenciosamente** — os dados (~38 GB) foram transferidos com sucesso pelos 12 streams, mas o backup **nunca foi commitado**. O diretório de storage confirma a ausência do backup do dia.

## Cronologia dos Eventos

| Horário | Evento |
|---------|--------|
| ~00:00 | Sessão inicia em modo paralelo (12 streams, ~22 MBps agregado) |
| 00:00–05:59 | Transferência estável, ~38 GB transferidos por stream |
| **05:59:40** | **i/o timeout** em múltiplos streams (0, 1, 4, 6, 8, 10) |
| 05:59:41–06:00:21 | Reconexões bem-sucedidas (re-join) nos streams afetados |
| 06:00:49 | Segunda onda de i/o timeouts (streams 3, 5, 8, 9) |
| 06:01:25–06:02:00 | Mais reconexões bem-sucedidas |
| **06:02:18–06:03:30** | **Todos os 12 streams completam** a transferência (~38 GB cada) |
| **06:08:12** | ⚠️ **Control channel fecha por EOF** |
| **06:14:30** | Agente reconecta o control channel, mas **não reassocia à sessão** |
| **07:08:07** | `CleanupExpiredSessions` remove a sessão (age=7h, idle=1h05min) |

## Causa Raiz

O problema está no **ciclo de vida da conexão primária (control channel) vs. fluxo da sessão paralela**.

### O que aconteceu no código

O fluxo da sessão é orquestrado em [handleParallelBackup](file:///home/lucas/Projects/n-backup/internal/server/handler_parallel.go#L120-L358):

```
1. Cria sessão → 2. Espera streams → 3. Espera ControlIngestionDone → 4. Finalize → 5. Commit
```

O passo 3 é um **select bloqueante** (linha 225):

```go
select {
case <-pSession.IngestionDone:   // ← NUNCA chegou
case <-pSession.Aborted:         // ← não foi abortado
case <-time.After(25 * time.Hour): // ← timeout de 25h
case <-ctx.Done():               // ← ctx da conn primária
}
```

> [!CAUTION]
> Quando o control channel caiu por EOF às **06:08**, o `ctx` da conexão primária foi cancelado. Mas isso depende de como o `ctx` é gerenciado. Se a goroutine de `handleParallelBackup` estava esperando no select e o `ctx.Done()` não foi acionado corretamente (o control channel não tinha listener ativo lendo), a sessão ficou **presa eternamente** no select esperando `IngestionDone` que nunca chegou — até ser removida pelo cleanup.

### Dois bugs identificados

**1. Control channel não é resiliente a queda durante streaming ativo**

O protocolo atual assume que a conexão primária (control channel) permanece estável durante toda a transferência. Quando ela cai:
- O agente reconecta o control channel, mas o novo canal **não é associado à sessão existente**
- O `ControlIngestionDone` que o agente envia na reconexão vai para uma nova goroutine que não tem contexto da sessão

**2. `CleanupExpiredSessions` não registra sessão expirada no histórico**

Em [handler_storage.go:150-173](file:///home/lucas/Projects/n-backup/internal/server/handler_storage.go#L150-L173), quando uma `ParallelSession` expira:

```go
s.Closing.Store(true)
// cancela slots, faz cleanup do assembler...
sessions.Delete(key)  // ← deleta sem registrar no histórico!
```

O `recordSessionEnd()` só é chamado no caminho feliz (linha 338 de `handler_parallel.go`), nunca no cleanup.

## Impacto

- **Perda de backup**: ~38 GB transferidos, ~5 horas de sessão, descartados
- **Sem visibilidade**: sessão não aparece no histórico (Dashboard), impossível detectar a falha sem olhar logs do systemd
- **Próximo backup**: depende do agendamento do agente

## Recomendações de Correção

1. **Control channel resiliente**: implementar mecanismo de reassociação do control channel à sessão existente após reconexão, similar ao que já existe para os data streams (ParallelJoin)
2. **Registrar sessões expiradas**: no `CleanupExpiredSessions`, chamar `recordSessionEnd()` com resultado `expired` antes de deletar
3. **Evento de alerta**: emitir evento `session_expired` no `EventStore` para visibilidade no dashboard
4. **Monitorar control channel na goroutine de handshake**: detectar queda do control channel mais cedo e propagar o `ctx.Done()` para o select que espera `IngestionDone`
