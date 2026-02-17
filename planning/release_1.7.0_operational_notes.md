# Notas Operacionais — Release 1.7.0

## Contexto

Durante a sequência de releases (`v1.6.2` e `v1.7.0`), observamos um cenário real de reconnect contínuo em stream paralelo (`stream 11`) com falha final por `ParallelStatusNotFound (status=2)` no agent.

## Causa Raiz Confirmada

- O server encerrava a sessão paralela cedo em condição de corrida:
  - `handleParallelBackup` podia entrar em `Wait()` antes do `StreamWg.Add(1)` do primeiro join.
  - A sessão era removida e reconnects seguintes recebiam `parallel session not found`.

## Correção Aplicada

- Commit: `62df5b3`
- Mudança: ordem de sincronização em `internal/server/handler.go`
  - `StreamWg.Add(1)` passa a ocorrer antes de sinalizar `StreamReady`.

## Evidência de Produção (padrão de log)

Sequência que confirma o bug antigo:
1. `all parallel streams complete`
2. `parallel assembly complete, waiting for trailer`
3. ainda chegam `parallel join request received` da mesma sessão
4. depois `parallel session not found` (agent vê `status=2`)

## Lições para próximas releases

1. Evitar retag com mesma versão para correção crítica em produção.
2. Preferir bump de patch (`vX.Y.Z+1`) para garantir distinção em pacote (`dpkg`).
3. Em casos de retag inevitável, validar hash do binário instalado vs artefato da release.

## Checklist de validação pós-release (paralelo)

1. `go test ./...` local antes da tag.
2. Validar `internal/integration TestEndToEnd_ParallelBackupSession` com repetição (`-count`).
3. Conferir workflow `Release` em todas arquiteturas.
4. No server em produção, monitorar:
   - `parallel session not found`
   - `all parallel streams complete`
   - `parallel assembly complete, waiting for trailer`
   - reconnect storm por stream específico.

## Estado atual

- `v1.6.2` foi republicada com fix.
- `v1.7.0` publicada após suíte completa verde, incluindo observability.
