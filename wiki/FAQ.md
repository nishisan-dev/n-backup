# FAQ — Perguntas Frequentes e Troubleshooting

## Geral

### O n-backup faz backup incremental?

Não na v1. Cada execução gera um arquivo `.tar.gz` completo. Backup incremental/diferencial está fora do escopo atual.

### Preciso de SSH para usar o n-backup?

Não. O n-backup usa TCP puro com mTLS (TLS 1.3). Não depende de SSH, shell remoto ou qualquer acesso ao filesystem do server além da porta configurada (default `9847/tcp`).

### É possível fazer restore via n-backup?

Na v1, o restore é manual. Os backups são arquivos `.tar.gz` padrão:

```bash
# Listar conteúdo
tar tzf backup.tar.gz

# Restaurar completo
tar xzf backup.tar.gz -C /restore/path

# Restaurar arquivo específico
tar xzf backup.tar.gz -C /restore/path path/to/file.txt
```

### Qual o formato de saída?

`.tar.gz` padrão Linux (ou `.tar.zst` se o storage estiver configurado com `compression_mode: zst`). Compatível com `tar xzf` ou `tar --zstd -xf`.

---

## Conexão e TLS

### Erro: "tls: bad certificate" ou "certificate signed by unknown authority"

O agent e o server devem ter certificados assinados pela **mesma CA**. Verifique:

1. Ambos referenciam o mesmo arquivo `ca.pem`
2. O certificado do agent foi assinado pela mesma CA que o do server
3. Os certificados não estão expirados (`openssl x509 -in cert.pem -noout -dates`)

### Erro: "agent name mismatch" ou "handshake rejected"

O **Common Name (CN)** do certificado do agent deve ser **idêntico** ao campo `agent.name` no `agent.yaml`.

```bash
# Verificar o CN do certificado
openssl x509 -in agent.pem -noout -subject
# Output esperado: subject= /CN=web-server-01

# No agent.yaml:
agent:
  name: "web-server-01"   # ← deve ser idêntico ao CN
```

### Erro: "connection refused" ou timeout

1. Verifique se o server está rodando: `systemctl status nbackup-server`
2. Verifique a porta: `ss -tlnp | grep 9847`
3. Verifique firewall: `iptables -L -n | grep 9847`
4. Teste conectividade: `nbackup-agent health <server>:9847 --config agent.yaml`

### O health check retorna "LOW DISK"

O server detectou pouco espaço livre no filesystem do storage. Libere espaço ou reduza `max_backups`.

---

## Backup e Performance

### O backup é muito lento

Possíveis causas e soluções:

| Causa | Solução |
|-------|---------|
| Link WAN com latência alta | Use `parallels: 2-4` para múltiplos streams |
| Disco de origem lento | Nada a fazer no lado do n-backup — I/O bound |
| Disco de destino lento (USB/HDD) | Use `assembler_mode: lazy` no storage |
| Bandwidth throttle ativo | Aumente ou remova `bandwidth_limit` |
| Muitos arquivos pequenos | Normal — overhead de headers tar. Considere excluir caches/logs |

### "CHECKSUM_MISMATCH" no Final ACK

O SHA-256 calculado pelo agent diferiu do server. Possíveis causas:

1. **Corrupção de rede** — improvável com TLS, mas possível
2. **Bug** — reporte no GitHub com logs de ambos os lados

O server descarta automaticamente o arquivo corrompido.

### O backup reinicia do zero em vez de fazer resume

O resume depende do **ring buffer em memória**. Verifique:

1. O `buffer_size` é grande o suficiente para a duração da queda
2. A sessão não expirou no server (TTL = 1h)
3. O agent não foi reiniciado (o buffer é volátil)

Para backups de 700GB+, considere `resume.buffer_size: 1gb`.

### Erro: "STORAGE_NOT_FOUND"

O `storage` referenciado no agent não existe na configuração do server. Verifique:

```yaml
# agent.yaml
backups:
  - name: app
    storage: scripts    # ← este nome...

# server.yaml
storages:
  scripts:              # ← ...deve existir aqui
    base_dir: /var/backups/scripts
```

### Erro: "BUSY"

Já existe um backup deste agent para o mesmo storage em andamento. Aguarde a conclusão ou verifique se há processos travados:

```bash
# No server
journalctl -u nbackup-server --since "1 hour ago" | grep "web-server-01"
```

---

## Parallel Streaming

### Quando devo usar parallel streaming?

| Cenário | Recomendação |
|---------|-------------|
| LAN (< 1ms latência) | `parallels: 0` (single stream geralmente satura o link) |
| WAN (> 10ms latência) | `parallels: 2-4` (compensa latência com concorrência) |
| Link saturado | `parallels: 0` + `bandwidth_limit` para evitar impacto |
| Backups > 100GB | `parallels: 4-8` + `auto_scaler: adaptive` |

### "ErrAllStreamsDead"

Todos os streams paralelos falharam após esgotar as tentativas de reconnect (3 por stream). Verifique:

1. Conectividade de rede
2. Logs do server para erros de I/O
3. Se o server está sobrecarregado (muitas sessões simultâneas)

### O auto-scaler não está escalando

O auto-scaler tem histerese (3 janelas consecutivas) para evitar oscilação. Se o throughput é estável, pode não haver necessidade de escalar.

---

## Control Channel

### Para que serve o control channel?

- **Keep-alive**: Detecta desconexões proativamente
- **RTT**: Medição contínua de latência para monitoramento
- **Status**: Recebe carga do server e espaço livre em disco
- **Flow Rotation**: Server pode solicitar drenagem graceful de streams

### Posso desabilitar o control channel?

Sim. Defina `enabled: false` no `agent.yaml`. O agent funcionará normalmente, mas sem keep-alive, RTT e flow rotation graceful.

### O control channel está desconectando frequentemente

Verifique o `keepalive_interval`. Se for muito baixo em links instáveis, aumente para 60s ou mais:

```yaml
daemon:
  control_channel:
    keepalive_interval: 60s
    max_reconnect_delay: 10m
```

---

## WebUI

### A WebUI não carrega

1. Verifique se `webui.enabled: true` no `server.yaml`
2. Verifique se a porta está acessível (default `:8080`)
3. Verifique se seu IP está nos `allowed_cidrs`

### A WebUI mostra "No sessions"

Normal se não houver backups em andamento. Os dados são mantidos em memória — ao reiniciar o server, o histórico é perdido.

### As métricas do agent (CPU, memória, disco) não aparecem

Estas métricas são enviadas via control channel. Verifique se o control channel está ativo e conectado.

---

## Systemd

### Hot reload (recarregar configuração sem downtime)

```bash
sudo systemctl reload nbackup-agent
# ou
sudo kill -SIGHUP $(pgrep nbackup-agent)
```

O agent recarrega a configuração YAML sem interromper o backup em andamento.

### Logs do systemd

```bash
# Agent
journalctl -u nbackup-agent -f

# Server
journalctl -u nbackup-server -f

# Últimas 100 linhas
journalctl -u nbackup-server -n 100 --no-pager
```

---

## Referências

- [[Instalação]] — Build, PKI, certificados mTLS, systemd
- [[Guia de Uso|Guia-de-Uso]] — Comandos, daemon, retry, rotação, configuração detalhada
- [[Configuração de Exemplo|Configuração-de-Exemplo]] — Arquivos YAML completos
- [[Arquitetura]] — Visão técnica do sistema
- [[Especificação Técnica|Especificação-Técnica]] — Protocolo binário e frames
