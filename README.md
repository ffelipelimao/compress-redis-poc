# compress-redis-poc

POC para gravar um blob JSON de ~25 KB comprimido com **zstd** no Redis e descomprimir na leitura, antes do `json.Unmarshal`. O objetivo é reduzir os bytes que saem do Redis a cada request. Os dados são sintéticos (foo/bar).

## Fluxo

| chave | conteúdo |
|---|---|
| `remote-config-hash:<player>` | hash do config do player |
| `remote-config:<hash>` | JSON (formato atual) |
| `remote-config:z:<hash>` | o mesmo JSON comprimido com zstd (formato novo) |

Pods em versões diferentes dividem o mesmo Redis durante o rollout. Por isso:

| pod | escrita (`WriteMode`) | leitura (`ReadMode`) |
|---|---|---|
| antigo | `WriteLegacy` | `ReadLegacy` |
| novo, durante o rollout | `WriteBoth` | `ReadZstdFallbackLegacy` |
| novo, com todos os stacks atualizados | `WriteZstd` | `ReadZstdFallbackLegacy` |

O `Retrieve` lê a chave `z:` primeiro. Se ela não existir ou não decodificar, cai na chave antiga e devolve a origem `legacy_fallback`.

- `internal/remoteconfig/store.go`: `Store` e `Retrieve`
- `internal/remoteconfig/codec.go`: encoder e decoder zstd (`github.com/klauspost/compress`)
- `internal/fakeconfig`: gerador determinístico do config foo/bar
- `cmd/poc`: relatório de medição

## Rodando

Requisitos: Docker com `docker compose` ou `docker-compose`.

```bash
make test    # testes de integração contra Redis 7.2 no compose (matriz de rollout)
make poc     # relatório: codecs, decoder, bytes out e CPU do Redis, CPU do pod
make bench   # go benchmarks do codec
make real CONFIG_FILE=/abs/path/config.json   # mede com um JSON real (montado read-only, não vai para a imagem)
make down
```

Flags extras com `ARGS`, por exemplo `make poc ARGS="-requests 50000 -concurrency 64"`. Veja `-help` para a lista completa.

O compose fixa o Redis na CPU `0` e o cliente nas CPUs `1-3`, para que um não roube CPU do outro durante a medição. Em máquinas com menos de 4 CPUs, ajuste com `REDIS_CPUSET=0 CLIENT_CPUSET=1 make poc`.

## O que os testes validam

`TestRolloutMatrix` grava com um modo de escrita e lê com um modo de leitura. Em todo caso de sucesso, também compara o config lido com o gravado:

| quem grava | quem lê | resultado esperado |
|---|---|---|
| pod antigo | pod novo | `legacy_fallback` |
| pod novo (dual write) | pod antigo | `legacy` |
| pod novo (dual write) | pod novo | `zstd` |
| pod novo, chave `z:` removida | pod novo | `legacy_fallback` |
| pod novo, chave `z:` corrompida | pod novo | `legacy_fallback` |
| só zstd | pod novo | `zstd` |
| só zstd | pod antigo | **miss**, por isso o dual write precisa durar até todos os stacks estarem na versão nova |

## Resultados (config sintético)

Ambiente: Docker (colima, 4 vCPU) em um Apple M4 Pro, Redis 7.2, 20.000 operações com 32 clientes concorrentes.

| | JSON | zstd (default) |
|---|---|---|
| tamanho do blob (`STRLEN`) | 25.074 B | 6.561 B (−73,8%) |
| bytes out por GET do blob | 25,1 KB | 6,6 KB |
| CPU do Redis por GET do blob | 7,0 µs | 4,8 µs (−30%) |
| CPU do pod por retrieve (2 GETs + decode + unmarshal) | 230 µs | 247 µs (+7%) |
| decode zstd por blob | – | ~27 µs |

O que os números mostram:

- Os bytes out (`INFO stats` → `total_net_output_bytes`) caem na mesma proporção do tamanho do blob.
- O Redis gasta menos CPU por GET porque escreve menos bytes no socket.
- No pod, o decode soma ~27 µs por blob. O custo dominante continua sendo o `json.Unmarshal` (~150–190 µs), que já existe hoje.
- Um `zstd.Decoder` de pacote com `DecodeAll` pode ser usado por várias goroutines ao mesmo tempo e não guarda estado entre requests. Criar um decoder por request custa ~95 KB e 25 alocações por chamada, contra ~27 KB e 1 alocação do decoder compartilhado.
- O config sintético comprime um pouco mais do que um config real com centenas de chaves distintas. Use `make real` para medir o seu.
- A latência no Docker local não representa tráfego entre AZs. O ganho real vem de transferir menos bytes.

## Conclusão

A proposta funciona e vale seguir:

1. **O rollout é seguro.** Com dual write (`WriteBoth`) e leitura com fallback (`ReadZstdFallbackLegacy`), pods antigos e novos convivem no mesmo Redis sem miss e sem diferença no config lido. Isso inclui chave `z:` expirada ou corrompida. O único caso que quebra é parar de gravar a chave antiga antes de todos os stacks estarem na versão nova, e o teste prova que ele dá miss.
2. **O ganho está nos bytes out.** O blob cai ~70–74% e o tráfego Redis → pod cai na mesma proporção. A CPU do Redis por GET do blob também cai ~30%.
3. **O custo no pod é pequeno.** São ~27 µs de decode por blob, ~7–10% a mais de CPU por retrieve. Esse custo é pequeno perto do `json.Unmarshal` que já roda hoje. O encode (~70 µs) só acontece no `Store`, quando o config muda.
4. **Implementação recomendada:**
   - zstd no nível default. Os níveis maiores ganham ~1–2 pontos de compressão e custam 4–5× mais no encode.
   - Um `zstd.Decoder` e um `zstd.Encoder` de pacote, usados com `DecodeAll`/`EncodeAll`. Não carregam estado de request e alocam bem menos do que criar um decoder por request.
   - Limite de memória no decoder (`WithDecoderMaxMemory`).
   - Fallback para a chave antiga também quando o decode falha, não só no miss.
5. **Ordem do rollout:**
   1. Deploy com `WriteBoth` e `ReadZstdFallbackLegacy`.
   2. Com todos os stacks na versão nova, deploy com `WriteZstd`.
   3. As chaves antigas expiram sozinhas pelo TTL.
6. **Falta confirmar** o tamanho real do blob em produção (`STRLEN remote-config:<hash>`) para fechar a estimativa de tráfego, porque a taxa de compressão depende do conteúdo do config.
