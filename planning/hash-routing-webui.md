# Hash Routing na WebUI — Refresh sem perder sessão

A SPA de observabilidade navega entre views via variáveis JavaScript em memória (`currentView`, `selectedSessionId`). Um F5 ou refresh sempre reseta para Overview, perdendo a posição do usuário.

## Proposta

Implementar **hash-based routing** usando `window.location.hash` para sincronizar o estado da URL com a navegação interna.

### Mapeamento de rotas

| Hash | View | Comportamento |
|---|---|---|
| `#overview` (ou vazio) | Overview | Padrão |
| `#sessions` | Lista de sessões | Lista |
| `#sessions/{id}` | Detalhe de sessão | Abre direto no detalhe |
| `#events` | Eventos | — |
| `#config` | Config | — |

---

## Proposed Changes

### WebUI JavaScript

#### [MODIFY] [app.js](file:///home/lucas/Projects/n-backup/internal/server/observability/web/js/app.js)

1. **Nova função `pushRoute(hash)`** — atualiza `window.location.hash` sem disparar re-render duplo
2. **Nova função `handleHashChange()`** — parseia a hash e chama `switchView` / `showSessionDetail` conforme necessário
3. **Modificar `switchView(view)`** — chamar `pushRoute` para sincronizar URL
4. **Modificar `showSessionDetail(id)`** — chamar `pushRoute` para sincronizar URL
5. **Listener `hashchange`** — escuta mudanças na hash (back/forward do browser)
6. **Init** — no boot, ler a hash atual para restaurar a view correta em vez de sempre começar em `overview`

### Lógica de Proteção

- Flag `isNavigating` para evitar loops entre `switchView` → `pushRoute` → `hashchange` → `switchView`
- Se a hash não for reconhecida, fallback silencioso para `#overview`

---

## Verificação

### Teste manual via Browser

1. Compilar e executar o servidor: `go run ./cmd/server`
2. Abrir a WebUI no browser (ex: `http://localhost:8080`)
3. **Caso 1 — Navegação entre tabs:**
   - Clicar em cada tab (Overview, Sessões, Eventos, Config)
   - Verificar que a URL muda para `#overview`, `#sessions`, `#events`, `#config`
4. **Caso 2 — Refresh preserva view:**
   - Navegar para Eventos (`#events`)
   - Pressionar F5
   - Verificar que a view Eventos continua ativa (não reseta para Overview)
5. **Caso 3 — Deep link para sessão:**
   - Se houver uma sessão ativa, clicar nela para ver detalhes
   - Verificar que a URL muda para `#sessions/{id}`
   - Pressionar F5
   - Verificar que o detalhe da sessão é restaurado
6. **Caso 4 — Botão voltar do browser:**
   - Navegar: Overview → Sessões → Eventos
   - Pressionar botão "voltar" do browser
   - Verificar que volta para Sessões, depois Overview
7. **Caso 5 — Hash inválida:**
   - Digitar manualmente `#xyz` na URL
   - Verificar que faz fallback para Overview
