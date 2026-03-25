# nbackup-server v4.0.1 — Flush Stall Detector

## Problema

Na sessão de backup do agente `popline-01` em **2026-03-25**, a ingestão de ~424 GB (12 streams × ~35.4 GB) completou com sucesso às **06:26:20**, mas o **flush final do chunk buffer antes do assembly falhou** com:

```
"flushing chunk buffer before finalize"
"chunk buffer flush timeout after 30s: 1675794175 bytes still in flight for session"
```

**~1.56 GB** de dados estavam pendentes no buffer. O disco (lento, HDD) estava escrevendo normalmente, mas o timeout absoluto de 30s matou o processo prematuramente. A sessão ficou presa por ~1h até o garbage collector limpá-la com `session_expired`.

## Causa Raiz

O timeout de flush no finalize usa um **deadline absoluto de 30s**. Em discos lentos com volumes grandes, esse tempo é insuficiente — mas o flush estava fazendo progresso (escrevendo em disco), apenas devagar.

## Mudança Requerida

### Escopo

- **APENAS** no flush que ocorre no finalize (após `ingestion_done_signal`), antes do assembly
- **NÃO** alterar o comportamento durante a transferência de dados (streams recebendo chunks) — continua como está

### De (comportamento atual)

```
flush_buffer()
if !completou_em_30s → ERROR "chunk buffer flush timeout"
```

### Para (stall detector baseado em progresso)

```
loop {
    bytes_antes = bytes_flushed
    espera(30s)  // ou intervalo configurável
    bytes_agora = bytes_flushed

    if bytes_agora == bytes_antes {
        // Zero progresso em 30s → ERRO legítimo, I/O está travado
        return error("flush stalled: no progress in 30s")
    }

    if bytes_agora >= total_esperado {
        // Flush completo com sucesso
        return ok
    }

    // Teve progresso mas ainda não terminou → continua monitorando
    log.info("flush in progress", bytes_flushed, bytes_remaining, elapsed)
}
```

### Lógica

| Cenário | Resultado |
|---------|-----------|
| Disco lento, flush de 1.56 GB em 3 min | ✅ OK — tem progresso contínuo |
| Disco travado (lock, hardware) | ❌ ERRO — 0 bytes em 30s |
| Flush completa rápido | ✅ OK — sai do loop imediatamente |

## Sessão de Referência (logs)

- **Agente:** `popline-01`
- **Session:** `a780ad5d-faf1-4c9f-b7aa-776cf3a26d99`
- **Server:** `mc-node-03` (`192.168.5.91`)
- **Erro:** `06:26:50` — flush timeout
- **Handshakes rejeitados:** `06:26:52–06:27:07` — 4× "backup already in progress"
- **Session expired:** `07:28:26` — idle 1h2m

## Release

Gerar release **v4.0.1** após a implementação.
