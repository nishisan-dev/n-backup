# Wiki GitHub — n-backup

Criar a pasta `wiki/` no repositório para publicação como GitHub Wiki (`nishisan-dev/n-backup.wiki.git`).

## Contexto

O projeto possui documentação rica em `docs/` (4 documentos + 5 diagramas PlantUML) além do `README.md`. A ideia é **reestruturar** esse conteúdo no formato do GitHub Wiki — uma página por arquivo `.md`, com uma `_Sidebar.md` para navegação.

> [!IMPORTANT]
> O GitHub Wiki é um repositório Git separado (`n-backup.wiki.git`). A pasta `wiki/` no repo principal servirá como **source of truth** para o conteúdo das páginas. A publicação final pode ser feita via clone do wiki repo ou via push direto.

## Estrutura Proposta

```
wiki/
├── Home.md                    ← Página inicial da wiki (overview do projeto)
├── Arquitetura.md             ← C4 Model, componentes, fluxos, decisões técnicas
├── Instalação.md              ← Build, PKI/mTLS, configuração, systemd
├── Guia-de-Uso.md             ← Comandos, daemon, retry, rotação, troubleshooting
├── Especificacao-Tecnica.md   ← Protocolo binário, frames, sessão, resume, parallel
├── Configuracao-de-Exemplo.md ← Exemplos completos de agent.yaml e server.yaml
├── WebUI.md                   ← Documentação do painel de observabilidade
├── FAQ.md                     ← Perguntas frequentes e troubleshooting consolidado
└── _Sidebar.md                ← Navegação lateral da wiki
```

> [!NOTE]
> No GitHub Wiki, os nomes dos arquivos definem os slugs das URLs. Hífens são exibidos como espaços no título. Exemplo: `Guia-de-Uso.md` → URL `/wiki/Guia-de-Uso` → título "Guia de Uso".

## Mapeamento do Conteúdo

| Página Wiki | Fonte | Transformação |
|---|---|---|
| `Home.md` | `README.md` | Versão condensada: overview, features, links para as demais páginas. Remover seções duplicadas (instalação, etc.) |
| `Arquitetura.md` | `docs/architecture.md` | Adaptar diagramas ASCII. Referenciar PlantUML via proxy `uml.nishisan.dev` |
| `Instalação.md` | `docs/installation.md` | Conteúdo integral com ajustes de links internos |
| `Guia-de-Uso.md` | `docs/usage.md` | Conteúdo integral, extrair FAQ para página dedicada |
| `Especificacao-Tecnica.md` | `docs/specification.md` | Conteúdo integral |
| `Configuracao-de-Exemplo.md` | `configs/*.example.yaml` | Ambos os YAMLs com comentários explicativos inline |
| `WebUI.md` | Seção de `docs/usage.md` | Extrair e expandir a seção sobre WebUI/Observabilidade |
| `FAQ.md` | Extraído de `docs/usage.md` | Consolidar troubleshooting + perguntas comuns |
| `_Sidebar.md` | Novo | Sidebar de navegação com links para todas as páginas |

## Detalhamento das Páginas

### `Home.md`
- Badge do CI/Release
- Descrição curta do projeto
- Tabela de features (resumida)
- Links rápidos: Download, Instalação, Arquitetura, Uso
- **NÃO duplicar** blocos de configuração ou instalação — apenas linkar

### `_Sidebar.md`
```markdown
## 📖 n-backup Wiki

- [[Home]]
- [[Arquitetura]]
- [[Instalacao]]
- [[Guia de Uso|Guia-de-Uso]]
- [[Especificação Técnica|Especificacao-Tecnica]]
- [[Configuração de Exemplo|Configuracao-de-Exemplo]]
- [[WebUI]]
- [[FAQ]]

---

**Links úteis**
- [📦 Releases](https://github.com/nishisan-dev/n-backup/releases)
- [📄 Código Fonte](https://github.com/nishisan-dev/n-backup)
```

### Diagramas PlantUML

Os diagramas serão referenciados via proxy no formato já padronizado:

```markdown
![Arquitetura](https://uml.nishisan.dev/proxy?src=https://raw.githubusercontent.com/nishisan-dev/n-backup/main/docs/diagrams/architecture.puml)
```

Os 5 diagramas existentes:
| Arquivo | Página destino |
|---|---|
| `architecture.puml` | Arquitetura |
| `c4_container.puml` | Arquitetura |
| `data_flow.puml` | Arquitetura |
| `parallel_sequence.puml` | Especificação Técnica |
| `protocol_sequence.puml` | Especificação Técnica |

## Decisões de Design

1. **Idioma:** PT-BR, consistente com a documentação existente
2. **Links internos:** Usar `[[Página]]` syntax nativa do GitHub Wiki
3. **Diagramas:** Manter nas `docs/diagrams/` do repo principal, referenciados via proxy — evita duplicação
4. **Config examples:** Manter em `configs/` do repo principal, copiar o conteúdo inline na página wiki para fácil consulta
5. **README.md principal:** Não será alterado — continuará como landing page do repositório

## Workflow de Publicação

Após criar a pasta `wiki/`, a publicação no GitHub Wiki pode ser feita de duas formas:

**Opção A — Push direto:** Clonar o wiki repo, copiar os arquivos e fazer push:
```bash
git clone https://github.com/nishisan-dev/n-backup.wiki.git
cp wiki/*.md n-backup.wiki/
cd n-backup.wiki && git add . && git commit -m "Sync wiki" && git push
```

**Opção B — GitHub Actions:** Criar um workflow que sincroniza `wiki/` → wiki repo automaticamente (pode ser planejado futuramente).

## Verificação

### Manual
1. Verificar que todos os arquivos `.md` estão na pasta `wiki/`
2. Verificar que links `[[Page]]` estão corretos na `_Sidebar.md`
3. Verificar que os diagramas PlantUML renderizam via proxy
4. Após o push para o wiki repo, navegar pelo GitHub Wiki e confirmar navegação e renderização
