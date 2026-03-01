# Resiliência no Encerramento: Aguardar `ChunkSACK` Final Antes do Shutdown

## Contexto

Na sessão `7460c1a5-6ac9-436c-b513-03b6c7e99b87`, o server identificou 5 chunks faltantes no fim do backup:

- `435562`
- `435573`
- `435574`
- `435585`
- `435586`

Os logs mostram o padrão abaixo:

1. O server recebeu `chunk_receive_started` para `435573` e `435562`
2. O payload desses dois chunks falhou com `connection reset by peer`
3. O agent enviou `ControlIngestionDone`
4. O server ainda estava recebendo dados de streams secundários depois disso
5. O assembly ficou com buracos permanentes

O ponto decisivo: o agent considerou a ingestão concluída antes de o server confirmar, via `ChunkSACK`, que os últimos bytes realmente tinham sido drenados.

---

## Causa Raiz

O fluxo atual do agent trata um chunk como "entregue" cedo demais:

1. `conn.Write()` retorna sucesso
2. O sender avança `sendOffset`
3. Quando o `RingBuffer` fecha e não há mais bytes novos para ler, o sender sai com `nil`
4. O `defer` do sender fecha a conexão
5. `WaitAllSenders()` retorna `nil`
6. O fluxo principal envia `ControlIngestionDone`

Isso acontece **sem esperar o `ChunkSACK` final** que avança o `tail` do `RingBuffer`.

Em outras palavras:

- `sendOffset` significa "bytes escritos no socket local"
- `rb.Tail()` significa "bytes realmente confirmados pelo server"

Hoje o shutdown depende do primeiro sinal, mas a segurança do protocolo depende do segundo.

---

## Por Que a Proposta Anterior Não Basta

A proposta baseada em `ErrBufferClosed during retry` cobre um subcaso real, mas não resolve este incidente específico.

Neste caso:

- não houve log de `stream write failed, attempting reconnect`
- não houve log de `reconnect failed`
- não houve log de `dead stream sender finished with error`

Isso indica que o agent **nao detectou** uma falha local de `write` nem entrou no fluxo de retry. O problema ocorreu depois do `write` parecer bem-sucedido localmente, enquanto bytes ainda estavam em voo ou pendentes de ACK no lado do server.

Logo, o bug principal nao e "close durante retry"; e "shutdown antes do drain final por `ChunkSACK`".

`ErrClosedDuringRetry` ainda pode ser uma defesa adicional, mas nao deve ser a correcao principal.

---

## Solução Ajustada

### Objetivo

So permitir que um stream termine com sucesso quando **todos os bytes que ele enviou ja tiverem sido confirmados por `ChunkSACK`**.

Critério de sucesso por stream:

- `rb.Tail() == rb.Head()`

Isto significa:

- nao existe mais dado pendente no `RingBuffer`
- o server ja confirmou todo o byte-stream daquele stream

Enquanto isso nao for verdade, o agent deve manter o stream vivo (ou reconectar) em vez de declarar sucesso.

---

## Proposed Changes

### Agent — Final Drain por Stream

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

**1. Adicionar estado explicito de "draining after EOF" no sender**

Quando `readChunkFrame()` retornar `ErrBufferClosed`, o sender nao deve sair imediatamente.

Novo comportamento:

- Se `rb.Tail() == rb.Head()`: retorno `nil` (stream realmente drenado)
- Se `rb.Tail() < rb.Head()`: entrar em uma fase de `final drain wait`

Essa fase representa:

- nao ha mais dados novos para produzir
- mas ainda ha bytes pendentes de confirmacao no server

**2. Implementar espera de drain final com timeout**

Novo helper no `Dispatcher` ou no `ParallelStream`, por exemplo:

```go
func (d *Dispatcher) waitForFinalDrain(stream *ParallelStream, timeout time.Duration) error
```

Responsabilidade:

- aguardar `rb.Tail() == rb.Head()`
- usar um timeout dinamico baseado em `sackTimeoutFn()`
- acordar periodicamente para reavaliar estado do stream

Se o drain completar:

- retornar `nil`

Se o timeout expirar:

- forcar reconexao do stream
- pedir `ParallelJoin` e usar `lastOffset` do server para retomar de onde ele realmente recebeu

**3. Reusar o mecanismo de resume no final da sessao**

Se o stream estiver em final drain e o server nao ACKar tudo a tempo:

- chamar `reconnectStream()`
- atualizar `sendOffset` para o `resumeOffset` retornado pelo server
- se `resumeOffset < rb.Head()`, reenviar os bytes pendentes

Isso fecha exatamente o buraco desta sessao:

- o `write` local pode ter retornado sucesso
- mas, se o server resetou no meio do payload, o `resumeOffset` volta antes do `head`
- o sender reenfileira implicitamente esses bytes via `sendOffset`

**4. Nao fechar a conexao no `defer` antes de confirmar drain**

Hoje o sender sempre fecha `stream.conn` ao sair.

Com a mudanca:

- ele so deve encerrar "com sucesso" depois do drain final
- o fechamento da conexao continua existindo, mas apenas apos `tail == head`

Na pratica:

- a conexao nao pode ser encerrada como parte de um EOF "normal" enquanto ainda houver bytes pendentes de ACK

**5. Expor helper de pendencia por stream**

Para deixar a logica de shutdown clara, adicionar um helper simples:

```go
func (s *ParallelStream) HasUnackedData() bool
```

Semantica:

- `true` quando `rb.Tail() < rb.Head()`
- `false` quando tudo que foi escrito ja foi confirmado

Isso evita espalhar comparacoes de `Tail()` e `Head()` em varios pontos.

---

### Agent — Barreira Antes de `ControlIngestionDone`

#### [MODIFY] [backup.go](file:///home/lucas/Projects/n-backup/internal/agent/backup.go)

**6. `WaitAllSenders()` so deve retornar sucesso quando todos os streams estiverem drenados**

O contrato de `WaitAllSenders()` deve mudar de:

- "todos os senders terminaram de escrever no socket"

para:

- "todos os senders terminaram e nao ha mais dados nao-ACKados em nenhum stream"

Com isso, o fluxo principal pode continuar como ja esta:

```go
sendersDone <- dispatcher.WaitAllSenders(sendersCtx)
if sendersErr != nil {
    return fmt.Errorf("parallel sender error: %w", sendersErr)
}
// seguro enviar ControlIngestionDone
```

O ponto importante e que `sendersErr == nil` passa a significar:

- nao existem bytes pendentes de `ChunkSACK`

**7. Se o drain final falhar, abortar o backup antes de `ControlIngestionDone`**

Exemplos:

- timeout no drain final e reconexao falha
- `resumeOffset` expirou do ring buffer
- nenhum stream consegue confirmar seus bytes finais

Nesses casos:

- retornar erro de `WaitAllSenders()`
- nao enviar `ControlIngestionDone`
- deixar o backup falhar explicitamente no agent

Isso e muito melhor do que aparentar sucesso e descobrir o buraco so no `Finalize()` do server.

---

### Agent — Defesa Adicional Opcional

#### [MODIFY] [dispatcher.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher.go)

**8. Manter `ErrClosedDuringRetry` como protecao complementar**

A ideia original de diferenciar:

- EOF normal
- buffer fechado durante retry/reconnect

continua util.

Mas aqui ela entra como **defesa em profundidade**, nao como mecanismo principal.

Se implementada:

- o sender que for interrompido no meio de um retry deve retornar erro
- `WaitAllSenders()` propaga esse erro

Isso ajuda a nao mascarar um shutdown durante reconexao, mas nao substitui a necessidade do `final drain`.

---

### Server — Guard de Diagnostico Antes do `Finalize`

#### [MODIFY] [handler.go](file:///home/lucas/Projects/n-backup/internal/server/handler.go)

**9. Adicionar validacao leve antes de entrar em `Finalize()`**

Como defesa em profundidade, o server pode detectar um estado obviamente incompleto antes de iniciar o I/O de `Finalize()`.

No caso de assembler lazy, se:

- `preStats.Phase == "receiving"`
- `preStats.PendingChunks != int(preStats.TotalChunks)`

entao ja existe evidencia de gap no conjunto recebido.

Acao sugerida:

- logar erro com `pending`, `totalChunks` e `nextExpectedSeq`
- abortar com `FinalStatusWriteError`

Observacao:

- isso e apenas um guard de diagnostico rapido
- a verificacao definitiva continua sendo a do `Finalize()`
- a checagem deve ser encapsulada em helper proprio do assembler se precisarmos evitar heuristica baseada em `Stats()`

---

## Fluxo Esperado Após a Correção

1. Produtor termina e chama `dispatcher.Flush()`
2. `dispatcher.Close()` sinaliza que nao havera novos dados
3. Cada sender entra em `final drain wait` ao encontrar EOF logico
4. O `ACK reader` continua processando `ChunkSACK`
5. Se todos os streams alcancarem `tail == head`, `WaitAllSenders()` retorna `nil`
6. So entao o agent envia `ControlIngestionDone`
7. Se algum stream nao drenar:
8. o sender reconecta e usa `resumeOffset`
9. os bytes faltantes sao reenviados
10. se ainda assim falhar, o backup aborta no agent com erro explicito

---

## Verification Plan

### Automated Tests

#### [MODIFY] [dispatcher_test.go](file:///home/lucas/Projects/n-backup/internal/agent/dispatcher_test.go)

**Teste 1: sender nao sai com `nil` se ainda houver bytes nao-ACKados**

- preencher um stream com dados
- avancar `sendOffset` ate `head`
- manter `tail < head`
- fechar o ring buffer
- verificar que o sender entra em espera de drain em vez de retornar sucesso imediato

**Teste 2: final drain completa quando chega `ChunkSACK` final**

- simular EOF no producer
- depois enviar `ChunkSACK` que leva `tail` ate `head`
- verificar que o sender termina com `nil`

**Teste 3: final drain com timeout aciona reconexao**

- simular EOF com bytes pendentes
- nao enviar `ChunkSACK`
- verificar que o sender chama o fluxo de reconexao
- servidor fake responde com `resumeOffset < head`
- verificar que o sender retoma do offset correto

**Teste 4: reconexao final falha e aborta o backup**

- simular timeout no final drain
- fazer `reconnectStream()` falhar
- verificar que `WaitAllSenders()` retorna erro
- verificar que o fluxo de backup nao chega em `ControlIngestionDone`

**Teste 5: `ErrClosedDuringRetry` permanece funcional (opcional)**

- validar o subcaso de buffer fechado no meio do retry
- garantir que ele continua retornando erro e nao `nil`

#### [MODIFY] [backup_test.go](file:///home/lucas/Projects/n-backup/internal/agent/backup_test.go)

**Teste 6: `ControlIngestionDone` so e enviado apos drain completo**

- usar dispatcher/control channel fake
- garantir que o envio do frame so acontece depois que todos os streams estiverem sem pendencias

#### [MODIFY] [handler_test.go](file:///home/lucas/Projects/n-backup/internal/server/handler_test.go)

**Teste 7: guard pre-finalize detecta conjunto incompleto**

- montar um assembler lazy com `PendingChunks < TotalChunks`
- verificar que o handler aborta antes de chamar `Finalize()`

---

## Verificação Manual

1. Reexecutar um backup paralelo em ambiente real com `logging.level: info`
2. Confirmar que `sent ControlIngestionDone to server` so aparece depois de todo `ChunkSACK` final dos streams
3. Rodar `scripts/check-missing-chunks.py` no session log do server
4. Confirmar ausencia de gaps

Para regressao do caso original:

- monitorar especialmente os streams que sofrem reset no fim da sessao
- validar que o agent reconecta ou espera o drain em vez de encerrar cedo

---

## Resultado Esperado

Com essa mudanca:

- o agent nao declara sucesso apenas porque conseguiu escrever bytes no socket local
- o encerramento passa a depender da confirmacao real do server via `ChunkSACK`
- resets tardios no fim da sessao deixam de causar perda silenciosa de chunks
- falhas finais passam a abortar explicitamente no agent, em vez de aparecer apenas como `missing chunk seq N in lazy assembly` no server
