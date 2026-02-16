# Plano — SPA de Observabilidade no Server (porta/IP configuráveis)

## Objetivo

Adicionar ao `nbackup-server` uma interface web (SPA) para observabilidade operacional:
- status geral do server
- sessões ativas (single e parallel)
- streams por sessão (throughput, estado, degradação)
- progresso e ETA (quando possível)
- eventos recentes (erros, reconnects, flow rotation)

Sem autenticação inicialmente, com controle de acesso por IP/CIDR (ACL) e bind em IP interno por padrão.

---

## Requisitos Funcionais

1. Expor SPA em listener HTTP separado do listener de backup TCP.
2. Configurar `host` e `port` do web listener no `server.yaml`.
3. Permitir ACL de origem por IP/CIDR.
4. Expor APIs de observabilidade para a SPA.
5. Atualização em tempo real via polling curto (v1) e opcional SSE/WebSocket (v2).
6. Exibir ETA apenas quando houver base confiável; caso contrário mostrar `N/A`.

---

## Requisitos Não Funcionais

1. Não impactar pipeline de backup (sem lock pesado nas hot paths).
2. Baixa alocação em snapshots de observabilidade.
3. Timeout e limites HTTP defensivos.
4. Sem dependências frontend pesadas na v1.
5. Compatível com deploy em rede interna sem autenticação.

---

## Proposta de Configuração

Adicionar no `internal/config/server.go`:

```yaml
web_ui:
  enabled: true
  listen: "127.0.0.1:9848"        # default recomendado (interno)
  read_timeout: 5s
  write_timeout: 15s
  idle_timeout: 60s
  allow_origins:                  # ACL por origem
    - "127.0.0.1/32"
    - "10.0.0.0/8"
```

Observações:
- Se `enabled=false`, nenhum listener HTTP é iniciado.
- ACL vazia: negar tudo (modo seguro).
- `allow_origins` aceita IP único (`192.168.1.10`) e CIDR (`192.168.1.0/24`).

---

## Arquitetura Técnica

## 1. Backend HTTP

Novo pacote sugerido:
- `internal/server/observability/`
  - `http.go` (router + middleware ACL)
  - `snapshot.go` (coleta lock-safe dos dados)
  - `dto.go` (tipos de resposta JSON)

Integração:
- `internal/server/server.go` inicia listener HTTP dedicado quando `web_ui.enabled=true`.
- compartilhar ponteiros somente-leitura para `Handler` e métricas atômicas já existentes.

## 2. SPA

Estratégia v1 (simples e robusta):
- SPA estática em `web/` (HTML/CSS/JS vanilla ou minimalista)
- embutida no binário com `go:embed`
- servida em `/` pelo listener HTTP da observabilidade

Vantagem:
- sem Nginx obrigatório
- deployment simples (um único binário)

## 3. ACL de Origem

Middleware antes dos handlers:
1. extrair IP remoto (`RemoteAddr`)
2. verificar contra lista parseada de CIDRs/IPs
3. negar com `403` quando não permitido

Sem autenticação na v1, conforme requisito.

---

## API de Observabilidade (v1)

`GET /api/v1/health`
- status do processo, uptime, versão, build info.

`GET /api/v1/metrics`
- counters globais já existentes:
  - throughput in/out
  - bytes escritos em disco
  - taxa média por janela

`GET /api/v1/sessions`
- lista de sessões ativas:
  - `session_id`, `agent`, `storage`, `mode`, `started_at`, `last_activity`
  - `bytes_received`
  - `active_streams` (parallel)
  - `status` (`running`, `degraded`, `idle`, `closing`)

`GET /api/v1/sessions/{id}`
- detalhes da sessão:
  - offsets por stream
  - throughput por stream (janela atual + média curta)
  - slow markers / flow rotation cooldown
  - retries/rejoins observados

`GET /api/v1/events?limit=200`
- ring buffer de eventos operacionais recentes (em memória):
  - reconnect
  - stream dead
  - rotate request/ack/timeout
  - checksum mismatch

`GET /api/v1/config/effective`
- visão efetiva de configuração relevante (sem segredos), útil para diagnóstico.

---

## ETA: Estratégia

ETA confiável depende de total esperado. Em backup streaming isso nem sempre existe no início.

Regra proposta:
1. Se `total_estimado` disponível: `ETA = (total - recebido) / throughput_ewma`.
2. Se não houver total estimado: retornar `null` e exibir `N/A`.
3. Sempre expor `throughput_ewma` e `bytes_received` para transparência.

Fonte de total estimado (ordem de prioridade):
1. metadado explícito enviado pelo agent (futuro)
2. heurística opcional baseada em histórico do mesmo job
3. sem dado => `N/A`

---

## UX da SPA (v1)

Páginas:
1. **Overview**
   - cards: sessões ativas, throughput total, streams ativos, alertas
   - gráfico simples de throughput (últimos N minutos)
2. **Sessões**
   - tabela com filtro por agent/storage/status
   - coluna de ETA (`N/A` quando indisponível)
3. **Detalhe da Sessão**
   - timeline de eventos
   - streams e offsets
   - indicadores de degradação/rotação
4. **Config/Diag**
   - config efetiva relevante
   - estado do control channel por agent

Atualização:
- polling a cada 2s na v1
- backoff automático quando a aba está em background

---

## Segurança Operacional (sem auth)

1. Default bind interno: `127.0.0.1:9848`.
2. ACL obrigatória (deny-by-default).
3. Sem endpoints mutáveis na v1 (somente leitura).
4. Sanitizar payloads e limitar tamanho de resposta.
5. Rate limit simples por IP (opcional recomendado).

---

## Plano de Implementação (Fases)

## Fase 1 — Config + Listener + ACL

Escopo:
- `internal/config/server.go`: bloco `web_ui` + validação
- `internal/server/server.go`: iniciar/parar listener HTTP
- middleware ACL por CIDR/IP

Teste:
- unit para parse/validação de config
- unit ACL (`allow`/`deny`, IPv4/IPv6)
- smoke de start/stop do listener HTTP

## Fase 2 — Snapshot e APIs base

Escopo:
- snapshot de sessões/métricas lock-safe
- endpoints:
  - `/api/v1/health`
  - `/api/v1/metrics`
  - `/api/v1/sessions`
  - `/api/v1/sessions/{id}`

Teste:
- unit dos DTOs e serialização
- testes HTTP com `httptest`
- race test em snapshot (`go test -race` neste pacote)

## Fase 3 — Eventos + ETA + refinamentos

Escopo:
- ring buffer de eventos em memória
- endpoint `/api/v1/events`
- ETA com política `N/A` quando indisponível

Teste:
- unit de ring buffer
- unit de cálculo ETA e casos sem total

## Fase 4 — SPA v1 embutida

Escopo:
- `web/` com páginas Overview/Sessions/SessionDetail
- `go:embed` no servidor HTTP
- polling e estados de erro/reconexão

Teste:
- smoke de serving de assets
- validação manual de UX em desktop/mobile

## Fase 5 — Hardening e rollout

Escopo:
- limites HTTP (`ReadHeaderTimeout`, `MaxHeaderBytes`, timeouts)
- documentação operacional
- checklist de produção

Teste:
- carga leve de polling concorrente
- validação de impacto zero no throughput de backup

---

## Mudanças de Config/Docs

Arquivos previstos:
- `configs/server.example.yaml` (novo bloco `web_ui`)
- `docs/installation.md` (porta interna + ACL)
- `docs/usage.md` (endpoints e exemplos)
- `packaging/systemd/nbackup-server.service` (sem mudança obrigatória; apenas documentação de porta)

---

## Riscos e Mitigações

1. **Risco**: snapshot custoso sob alta concorrência.
   - Mitigação: coletar via atômicos/sync.Map e evitar locks globais.

2. **Risco**: exposição indevida sem auth.
   - Mitigação: bind interno por default + ACL deny-by-default.

3. **Risco**: UI induzir interpretação errada de ETA.
   - Mitigação: ETA opcional com `N/A` explícito e tooltip da fonte.

4. **Risco**: aumento de complexidade no binário server.
   - Mitigação: pacote isolado `observability` e APIs read-only.

---

## Entregáveis de v1

1. SPA embutida com visão operacional básica.
2. APIs JSON read-only de status/sessões/métricas.
3. Configurabilidade completa de bind IP/porta.
4. ACL por IP/CIDR funcionando e testada.
5. Documentação de deploy interno.

---

## Recomendação de Deploy Inicial

Para o seu cenário atual:
- bind em IP interno (`10.x.x.x`) ou localhost com túnel
- sem autenticação
- ACL restrita à rede de operação

Exemplo:

```yaml
web_ui:
  enabled: true
  listen: "10.0.0.12:9848"
  allow_origins:
    - "10.0.0.0/24"
```

