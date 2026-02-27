# Plano de Cobertura de Documentação (Audit 2026-02-24)

## Objetivo

Mapear lacunas e inconsistências da documentação atual (`README`, `docs/`, `wiki/`, exemplos YAML) e definir um backlog priorizado do que precisa ser atualizado.

## Escopo Revisado

1. `README.md`
2. `docs/architecture.md`
3. `docs/installation.md`
4. `docs/usage.md`
5. `docs/specification.md`
6. `wiki/*.md` (principalmente `Guia-de-Uso`, `Especificacao-Tecnica`, `Configuracao-de-Exemplo`, `Instalação`)
7. `configs/server.example.yaml` e `configs/agent.example.yaml` como fonte de configuração efetiva
8. Referência de implementação: `internal/config/*.go`, `internal/protocol/control.go`, `internal/server/chunkbuffer.go`

## Achados Priorizados

| Prioridade | Achado | Impacto | Evidência |
|---|---|---|---|
| P0 | YAML inválido/desatualizado em docs de uso/instalação (`daemon.schedule`) | Usuário copia configuração que não reflete o schema atual | `docs/usage.md:33`, `docs/installation.md:182` |
| P0 | `docs/specification.md` ainda descreve `storage:` (singular) em vez de `storages:` | Especificação contradiz configuração real do server | `docs/specification.md:593` |
| P1 | Limite de streams documentado como `1-8`, mas código aceita `1-255` | Tuning incorreto de paralelismo | `docs/specification.md:291`, `docs/specification.md:351`, `wiki/Especificacao-Tecnica.md:291`, `wiki/Especificacao-Tecnica.md:358`, `internal/config/agent.go:161` |
| P1 | Feature `chunk_buffer` sem cobertura em `docs/`/`wiki/` | Feature nova sem guia operacional/tuning | Sem ocorrências de `chunk_buffer`/`drain_ratio` em `docs/*.md` e `wiki/*.md`; config real em `configs/server.example.yaml:62` |
| P1 | Frame `ControlIngestionDone (CIDN)` existe no protocolo e fluxo do server, mas não está na especificação | Protocolo documentado incompleto | `internal/protocol/control.go:44`, `internal/server/handler.go:1270` |
| P1 | Campo `dscp` existe no `agent.yaml` (schema), mas não está documentado para operadores | Recurso sem adoção por falta de documentação | `internal/config/agent.go:67` |
| P2 | Divergência entre `docs/` e `wiki/` (conteúdo duplicado e inconsistente) | Alta chance de regressão documental | `git diff --no-index --stat` mostra diferenças relevantes nos 4 pares principais |
| P2 | Comentário legado de `drain_mode` no exemplo de config do server | Confusão conceitual com `drain_ratio` | `configs/server.example.yaml:58` |

## Documentos Que Precisam Ser Abordados

### Fase 1 (Correção de verdade funcional)

1. `docs/usage.md`
2. `docs/installation.md`
3. `docs/specification.md`
4. `wiki/Especificacao-Tecnica.md`
5. `configs/server.example.yaml` (comentário legado)

### Fase 2 (Cobertura de features atuais)

1. `README.md` (menção curta de `chunk_buffer` e link para guia)
2. `docs/usage.md` (seção completa de `chunk_buffer`)
3. `wiki/Guia-de-Uso.md` (espelhar seção operacional)
4. `wiki/Configuracao-de-Exemplo.md` (bloco `chunk_buffer` em `server.yaml`)
5. `docs/specification.md` e `wiki/Especificacao-Tecnica.md` (adicionar `ControlIngestionDone`)
6. `docs/diagrams/protocol_sequence.puml` e/ou `docs/diagrams/parallel_sequence.puml` (incluir `CIDN`)
7. `docs/usage.md` e `wiki/Guia-de-Uso.md` (adicionar `dscp`)
8. `wiki/Cenarios-de-Configuracao.md` (cenário recomendado para `chunk_buffer` em HDD/NAS)

### Fase 3 (Governança e anti-drift)

1. Definir fonte única de verdade: `docs/` ou `wiki/`
2. Criar regra de sincronização (script/CI) para evitar drift entre pares:
   - `docs/usage.md` ↔ `wiki/Guia-de-Uso.md`
   - `docs/specification.md` ↔ `wiki/Especificacao-Tecnica.md`
   - `docs/architecture.md` ↔ `wiki/Arquitetura.md`
   - `docs/installation.md` ↔ `wiki/Instalacao.md`
3. Adicionar checklist de release para validação documental (schema + exemplos + limites)

## Plano de Execução Proposto

1. PR 1 (P0): corrigir inconsistências de schema e ranges (`daemon.schedule`, `storage`, `1-8`).
2. PR 2 (P1): documentar `chunk_buffer`, `CIDN` e `dscp` em `docs/` e `wiki/`.
3. PR 3 (P2): reduzir duplicação e institucionalizar sincronização entre `docs/` e `wiki/`.

## Critério de Pronto

1. Todos os snippets YAML de `docs/` e `wiki/` validam contra o schema atual (`internal/config`).
2. Não existem referências a `1-8` para `parallels` na documentação de usuário/especificação.
3. `chunk_buffer` está documentado com semântica de `size` e `drain_ratio`.
4. `ControlIngestionDone` aparece na especificação textual e em pelo menos um diagrama de sequência.
5. Existe política explícita de source-of-truth para documentação duplicada (`docs/` vs `wiki/`).
