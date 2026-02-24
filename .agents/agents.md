# Instruções de Documentação — n-backup

## Fonte de Verdade

**`docs/` é a fonte canônica.** A `wiki/` é o espelho operacional.

Toda alteração de documentação começa em `docs/`. A wiki é atualizada **no mesmo commit**.

---

## Pares de Arquivos (docs/ ↔ wiki/)

| Fonte canônica | Espelho wiki |
|----------------|-------------|
| `docs/usage.md` | `wiki/Guia-de-Uso.md` |
| `docs/specification.md` | `wiki/Especificação-Técnica.md` |
| `docs/architecture.md` | `wiki/Arquitetura.md` |
| `docs/installation.md` | `wiki/Instalação.md` |

**Regra:** qualquer alteração de conteúdo em um arquivo de `docs/` deve ser espelhada no arquivo correspondente da `wiki/`, e vice-versa.

Exceções permitidas na wiki (não existe em docs/):
- Links internos no formato `[[Texto|Página]]`
- Referências cruzadas entre páginas da wiki (`[[Configuração de Exemplo|...]]`)

---

## Formatação

| Elemento | docs/ | wiki/ |
|----------|-------|-------|
| Alertas | `> [!NOTE]` / `> [!TIP]` / `> [!IMPORTANT]` / `> [!WARNING]` | Mesmo padrão GFM |
| URLs de exemplo | `backup.example.com` (nunca `backup.nishisan.dev`) | Mesmo |
| YAML de exemplo | Validado contra schema real | Mesmo |

---

## O que Atualizar ao Mudar o Código

### Ao adicionar/alterar um campo de configuração

- [ ] Atualizar `configs/agent.example.yaml` ou `configs/server.example.yaml`
- [ ] Atualizar `docs/usage.md` (seção relevante + tabela de parâmetros)
- [ ] Espelhar em `wiki/Guia-de-Uso.md`
- [ ] Atualizar `wiki/Configuração-de-Exemplo.md` (tabela de campos importantes)
- [ ] Verificar se `docs/specification.md` cobre o campo tecnicamente

### Ao adicionar um novo frame de protocolo

- [ ] Documentar em `docs/specification.md` (seção 3.x — Protocolo)
- [ ] Espelhar em `wiki/Especificação-Técnica.md`
- [ ] Incluir: magic bytes, estrutura do frame (ASCII art), campos e semântica

### Ao adicionar uma nova feature visível ao usuário

- [ ] Adicionar à tabela `## ✨ Features` do `README.md`
- [ ] Adicionar seção em `docs/usage.md`
- [ ] Espelhar em `wiki/Guia-de-Uso.md`
- [ ] Se arquitetural: atualizar `docs/architecture.md` e `wiki/Arquitetura.md`
- [ ] Se cenário de configuração aplica: adicionar em `wiki/Cenários-de-Configuração.md`

### Ao alterar limites/ranges validados no código

Exemplos: parallels (0-255), chunk_size (64kb-16mb), drain_ratio (0.0-1.0)

- [ ] Validar no código (`internal/config/agent.go` ou `internal/config/server.go`)
- [ ] Atualizar todos os lugares que mencionam o range antigo
- [ ] Executar: `bash scripts/check-doc-drift.sh` para verificar invariantes

---

## Invariantes a Manter

Estes valores estão validados no código e **nunca** devem estar documentados errado:

| Campo | Valor correto | Arquivo de referência |
|-------|--------------|----------------------|
| `parallels` range | `0-255` | `internal/config/agent.go` |
| `chunk_size` range | `64kb` a `16mb` | `internal/config/agent.go` |
| `bandwidth_limit` mínimo | `64kb` | `internal/config/agent.go` |
| `drain_ratio` range | `0.0` a `1.0` | `internal/config/server.go` |
| `schedule` campo | `backups[].schedule` (não `daemon.schedule`) | `internal/config/agent.go` |
| Schema de storages | `storages:` (plural) | `internal/config/server.go` |
| DSCP valores válidos | `EF`, `AF11-AF43`, `CS0-CS7` | `internal/agent/dscp.go` |

---

## Validação Anti-Drift

Antes de commitar alterações de documentação:

```bash
bash scripts/check-doc-drift.sh
```

O script verifica invariantes estruturais e detecta valores conhecidos incorretos.
Deve retornar exit 0 com `"All checks passed."`.

---

## Commits de Documentação

- **Commits atômicos**: uma responsabilidade por commit
- **Prefixo**: `docs:` para alterações de documentação pura
- **Escopo**: commitar sempre docs/ e wiki/ juntos quando o conteúdo for espelhado
- **Nunca** commitar em versão/tag sem a documentação atualizada

### Exemplos de mensagem de commit

```
docs: adiciona seção de DSCP Marking em usage.md e Guia-de-Uso.md
docs: corrige range parallels 1-8 → 1-255 em spec e wiki
docs: adiciona frame CIDN na seção 3.6 do protocolo
```

---

## Checklist Pré-Release

Antes de criar uma tag/release:

- [ ] `bash scripts/check-doc-drift.sh` — sem violações
- [ ] Nenhum range documentado contradiz o código (`grep -n "1-8" docs/ wiki/`)
- [ ] Features novas aparecem no `README.md` (tabela Features)
- [ ] `configs/server.example.yaml` e `configs/agent.example.yaml` sincronizados com schema
- [ ] `CHANGELOG.md` atualizado com a nova versão
