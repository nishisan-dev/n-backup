# Correção da Degradação e Timeouts no Backup Paralelo

Identificamos duas anomalias críticas relatadas nos logs de produção:
1. O erro de _pool_ de conexões `"parallel session not found"` no stream `0`.
2. O erro massivo de `"i/o timeout"` em múltiplas streams do servidor reportando a exata mesma contagem de bytes.

## Análise de Causa Raiz (Root Cause)

1. **"parallel session not found" (Stream 0 falhando na Iniciação)**:
   A extensão `ParallelInit` é despachada na conexão de controle (`stream 0`), contudo o Agente inicia o `ParallelJoin` nas portas de dados instantaneamente — sem aguardar a confirmação de que a sessão paralela foi plenamente ativada do lado do servidor no `ControlChannel`. Como resultado, a primeira thread do Agente tipicamente chega ao servidor antes mesmo do `ParallelInit` ter sido roteado e processado pelo handler. 

2. **Degradação e Timeouts (Exatos X Bytes nas Streams 1..11)**:
   A causa remonta ao ciclo de `Close()` na camada de roteamento (`dispatcher.go`) no Agente. 
   Quando o produtor de arquivos/tar conclui com sucesso, ele chama `dispatcher.Close()`, o que fecha a estrutura do RingBuffer e invoca forçadamente `conn.Close()` nas conexões TLS.
   Este "close abrupto" corta a rede das *sender goroutines* que ainda poderiam estar ativas ou tentando esvaziar os últimos bytes do buffer.
   - As *sender goroutines* interceptam `io.ErrClosedPipe` na hora do `conn.Write`, desencadeando uma **Reconexão de Falha de Rede (Retry)** cega.
   - Os Senders sofrem "backoff exponencial", invocam um novo `ParallelJoin` com sucesso (pois a sessão ainda existe no servidor).
   - O Servidor inicializa as goroutines `receiveParallelStream` com o offset persistido (`bytes: 1111499040`).
   - A sender do Agente finalmente entra no loop com a _nova conexão_, checa o `RingBuffer` e descobre que ele foi fechado (`ErrBufferClosed`). Então, a Sender **retorna da função e morre em silêncio**, abandonando e DEIXANDO ABERTA a conexão nova rumo ao Servidor.
   - O Agente agora só avança se reportando como "Completo" ao término das *senders*. Mas como as senders deixaram o _TCP socket_ pendurado de lado, o Servidor fica pendurado lá ouvindo por **30 segundos** até que o deadline de Read excede o `streamReadDeadline`.

## Proposed Changes

### `internal/server/gap_tracker.go`
Melhoria arquitetural focada em latência e proteção de memória para evitar a degradação silenciosa em sessões intensas com _out-of-order delivery_- intenso.
- Refatorar o mapa infinito `gt.completed` para evitar que ele inche ao tamanho O(N) de todo o histórico da sessão. O tracking passará a utilizar janelas contíguas via ponteiros para aliviar a memória consumida num backup de longo prazo.

### `internal/agent/dispatcher.go`
- **[MODIFY] [dispatcher.go](file:///home/g0004218/Projects/n-backup/internal/agent/dispatcher.go)**:
  1. No método abstrato `Close()`, remover integralmente a terminação sumária de sockets físicos (`d.streams[i].conn.Close()`). O Dispatcher é responsável unicamente de declarar o `RingBuffer.Close()`.
  2. Atualizar o goroutine da função `startSenderWithRetry` a utilizar um bloco `defer` para garantir que o _TCP handler do sender_ auto-extermine de maneira elegante `conn.Close()` invariavelmente antes do fim do tempo de vida útil do canal quando o RingBuffer esgota legalmente.

### `internal/agent/backup.go`
- **[MODIFY] [backup.go](file:///home/g0004218/Projects/n-backup/internal/agent/backup.go)**:
  1. Atrasar o Join de dados lendo o _response explícito_ (ACK) gerado pelo frame em `RunParallelBackup` após despachar `protocol.WriteParallelInit`. Isto assegurará que o servidor instanciou a Sessão no seu HashMap antes que qualquer `ActivateStream(0..X)` pise a parede do roteador. 
  2. Somente acionar loop de chamadas em paralelo após decodificar o `protocol.ReadParallelACK` para esse estágio.

## Verification Plan

### Testes Manuais (User Verification)
Após implementar, o usuário deve:
1. Executar o fluxo completo de *parallel_backup* com o Agente modificado.
2. Procurar nos logs contendo as mensagens `reading chunk header seq: read tcp ... i/o timeout` - se o plano mitigar com sucesso, elas não deverão existir ou sequer poluirão o log na conclusão.
3. Observar ativamente se `parallel session not found` é interceptado para confirmar correções nas proteções de handshake tardio.
