# Apuracao 2026 - 2o turno Presidente (TSE ao vivo)

Servidor web em Go puro (sem dependencias) que consome os arquivos oficiais
da Divulgacao de Resultados do TSE e exibe um painel em tempo real do
segundo turno presidencial Flavio Bolsonaro (PL/22) x Lula (PT/13).

## Rodar

    go build -o apuracao2026.exe .
    ./apuracao2026.exe

Abra http://127.0.0.1:8081 (ou 8080, padrao; variavel PORT muda a porta).

## Fonte de dados

- Resultado nacional (EA20):
  https://resultados.tse.jus.br/oficial/ele2026/6258/dados/br/br-c0001-e006258-u.json
- Andamento por UF (EA14):
  https://resultados.tse.jus.br/oficial/ele2026/6258/dados/br/br-e006258-ab.json

O TSE manteve os arquivos congelados desde a checagem de sábado (idg 3943321,
gerado 24/10/2026 19:19:13) até o início da apuracao; a partir das 17h de
domingo (25/10) os arquivos passam a ser regravados a cada carga de totalizacao.

O servidor consulta o TSE a cada 30 s. A pagina consulta o servidor a cada
10 s. A API responde em /api/results e /api/health. Nenhum numero e
inventado: enquanto o TSE nao publica, o painel mostra 0 e o aviso
"aguardando apuracao".

## Metricas exibidas

- Percentual de cada candidato: campo pvap (sobre votos validos), direto do TSE
- Probabilidade de vitoria: estimativa estatistica propria - modelo uniforme
  sobre os votos pendentes, P = Phi(vantagem / (pendentes/sqrt(12))). E uma
  curva de plausibilidade, nao projecao oficial.
- Votos para o segundo colocado empatar: pendentes_validos_estimados / 2 -
  vantagem / 2
- Evolucao: serie temporal dos percentuais coletados pelo proprio navegador
- Estados: lideranca e % por UF, dos 27 arquivos estaduais do TSE
- Fotos: https://resultados.tse.jus.br/oficial/ele2026/6258/fotos/br/{sqcand}.jpeg
  (padrao extraido do app oficial; sqcand vem no proprio -u.json)

