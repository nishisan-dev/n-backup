# Production Review - n-backup

Data da revisão: 15/02/2026

## Resumo Executivo

O projeto `n-backup` apresenta uma base sólida e madura para uso em produção, com arquitetura clara, foco em segurança (mTLS), operação bem definida e pipeline de entrega consistente (CI + release).

No estado atual, minha avaliação é:
- Prontidão geral: **Alta**
- Risco operacional: **Moderado**
- Principal lacuna: **cobertura e profundidade de testes nos fluxos críticos (agent/server)**

## Evidências Observadas

- Estrutura de projeto organizada por domínio (`agent`, `server`, `protocol`, `config`, `pki`).
- Documentação abrangente e coerente com implementação (`README.md`, `docs/architecture.md`).
- CI com `go vet` + `go test` em PR/push (`.github/workflows/ci.yml`).
- Release automatizado com build multiplataforma e pacotes `.deb` (`.github/workflows/release.yml`).
- Testes executados com sucesso localmente:
  - `go test ./...` -> **passou** em todos os pacotes testáveis.
  - `go test ./... -cover` -> cobertura heterogênea (detalhada abaixo).

## Pontos Fortes

1. Segurança por padrão
- mTLS obrigatório e validação de certificados.
- Sem dependência de shell remoto/SSH para transporte de backup.

2. Arquitetura técnica robusta
- Streaming sem arquivos temporários na origem.
- Escrita atômica no servidor.
- Suporte a resume e paralelismo com componentes dedicados.

3. Maturidade operacional
- Empacotamento `.deb` com systemd e man pages.
- Fluxo de release estruturado com checksums.

4. Qualidade de engenharia
- Código modular.
- Testes unitários e de integração existentes.

## Riscos e Lacunas

1. Cobertura de testes em áreas críticas ainda baixa
- `internal/agent`: **23.5%**
- `internal/server`: **15.9%**
- Risco: regressões em cenários de falha/reconexão/paralelismo podem passar despercebidas.

2. Observabilidade com ponto pendente
- Há `TODO` para medição real de espaço livre em disco (`internal/server/handler.go`).
- Risco: health check pode não refletir condição real do storage em produção.

3. Complexidade funcional elevada em fluxos de rede
- Resume mid-stream e parallel streaming adicionam alto número de estados.
- Risco: bugs de concorrência e edge cases sob instabilidade de rede.

## Recomendações Prioritárias

1. Fortalecer testes dos caminhos críticos (prioridade máxima)
- Focar em:
  - perda de conexão e retomada por offset;
  - rejoin de streams paralelos;
  - lock por `agent:storage:backup`;
  - integridade final (hash/trailer) sob falha parcial.

2. Completar health check de capacidade de disco
- Implementar coleta real de espaço livre (ex.: `statfs`) e expor no protocolo de health.

3. Definir um "gate" mínimo de qualidade para release
- Exemplo: cobertura mínima por pacote crítico e smoke tests de cenários de rede.

4. Incrementar testes de estresse/confiabilidade
- Cenários longos com latência/perda simulada e reconexão recorrente.

## Conclusão

O `n-backup` já tem fundamentos fortes de produto e engenharia para produção. A próxima evolução mais valiosa não é ampliar features, e sim **reduzir risco operacional** com testes mais profundos nas rotas críticas (`agent` e `server`) e fechar pendências de observabilidade.

Com esses ajustes, o projeto avança de “tecnicamente pronto” para “operacionalmente resiliente”.
