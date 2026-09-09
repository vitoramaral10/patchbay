# syntax=docker/dockerfile:1

# Imagem do patchbay — gateway MCP self-hosted.
#
# CGO_ENABLED=0: o driver SQLite (modernc.org/sqlite) é puro Go, então o binário
# não precisa de gcc nem de glibc. Ele terminaria numa distroless "static" de
# ~2 MB — e terminava, até 2026-09-09.
#
# # Por que esta imagem deixou de ser distroless
#
# O patchbay executa MCP de processo local (transporte STDIO) como processo
# filho: `npx -y algum-mcp-server`, `uvx outro`. A distroless não tem Node, não
# tem Python e não tem shell, então nenhum desses subia nela — o upstream
# entrava em backoff eterno com "npx: executable file not found in $PATH".
# Enquanto a biblioteca só listava servidores remotos isso não aparecia; quando
# ela passou a oferecer os ~11 mil servidores que só existem como pacote, virou
# o caminho normal.
#
# A troca é decisão do dono (2026-09-09) e tem preço, dito aqui para ninguém
# descobrir depois:
#
#   - A base sai de ~2 MB para ~332 MB.
#   - Entra um gerenciador de pacotes e um shell na imagem de runtime — as duas
#     coisas que a distroless existia para não ter.
#   - `npx -y` **baixa e executa código de terceiro em tempo de execução**. O que
#     roda ali não passou por nenhum build nosso, não está no SBOM da imagem e
#     não é varrido pelo trivy. Quem cadastra um MCP de processo local está
#     escolhendo rodar aquele pacote — a seção "Upstream STDIO" do README diz o
#     que isso implica.
#
# O que sobrou do endurecimento, e continua valendo: usuário não-root com UID
# numérico, arquivos da aplicação pertencendo a root, bits setuid removidos,
# rootfs somente leitura no compose e nenhum segredo em camada.
#
# Base do builder fixada por digest (golang:1.26-bookworm em 2026-09-08);
# atualize o digest ao trocar de versão do Go, não a esmo.
FROM golang:1.26-bookworm@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81 AS builder
WORKDIR /src

# Só os manifestos primeiro: `go mod download` fica numa camada que só muda
# quando go.mod/go.sum mudam, cacheada por cache mount do BuildKit.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSAO=dev
ARG COMMIT=desconhecido
ARG DATA=desconhecida

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w \
        -X github.com/vitoramaral10/patchbay/internal/platform/versao.Numero=${VERSAO} \
        -X github.com/vitoramaral10/patchbay/internal/platform/versao.Commit=${COMMIT} \
        -X github.com/vitoramaral10/patchbay/internal/platform/versao.Data=${DATA}" \
      -o /out/patchbay ./cmd/patchbay

# O uv entra só como fonte de dois binários estáticos. Estágio próprio, e não um
# download no runtime, para o que roda na imagem final ter vindo de um digest
# fixo — `curl | sh` de instalador seria a corrente de suprimentos mais frouxa
# possível dentro do build.
FROM ghcr.io/astral-sh/uv:bookworm-slim@sha256:22334efe746f1b69217d455049b484d7b8cacfb2d5f42555580b62415a98e0a3 AS uv

# Imagem final: Node (que traz npm e npx) mais os binários do uv.
#
# Debian slim e não Alpine de propósito: muitos MCP server publicam módulo
# nativo pré-compilado para glibc, e no musl o npm cairia para compilar da
# fonte — que exigiria python3, make e g++ aqui dentro, trocando 200 MB de base
# por um compilador na imagem de runtime. É o pior dos dois lados.
FROM node:24-bookworm-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e AS runtime

# O usuário do patchbay é criado com UID 65532 e não reaproveita o "node" (1000)
# da base: 65532 é o dono de /dados nas instalações que já existem, e trocar o
# UID aqui faria o SQLite falhar com "unable to open database file (14)" no
# primeiro boot depois da atualização.
#
# Sem home de verdade e sem shell de login: o processo não precisa de nenhum dos
# dois, e HOME aponta para /dados logo abaixo.
RUN groupadd --system --gid 65532 patchbay \
 && useradd  --system --uid 65532 --gid 65532 --no-create-home \
             --shell /usr/sbin/nologin patchbay

# Bits setuid/setgid fora. A base traz alguns (su, mount, passwd) que nada aqui
# usa, e cada um deles é uma primitiva de escalada a menos para quem conseguir
# execução dentro do container. Feito no mesmo RUN que cria o usuário para não
# render uma camada só para isso.
RUN find / -xdev -perm /6000 -type f -exec chmod a-s {} + 2>/dev/null || true

COPY --from=builder --chown=root:root /out/patchbay /patchbay
COPY --from=uv --chown=root:root /usr/local/bin/uv /usr/local/bin/uvx /usr/local/bin/

# /dados precisa EXISTIR na imagem, com dono 65532 — o VOLUME lá embaixo só o
# declara, não o cria. Volume nomeado montado sobre um caminho ausente da
# imagem nasce root:root 0755, e o processo não-root não consegue criar
# patchbay.db ali: o gateway morre em laço com "unable to open database
# file (14)" (SQLITE_CANTOPEN). Existindo o caminho na imagem, o Docker
# replica dono e modo dele ao inicializar o volume enquanto este está vazio —
# o que conserta também um volume já criado antes desta correção.
# Bind mount não passa por aqui: ali quem manda é o dono do diretório no host,
# que o operador precisa ajustar para 65532 (ver docker-compose.yml).
#
# O subdiretório cache/ vem junto porque o rootfs é somente leitura no compose:
# npx e uvx precisam de algum lugar gravável, e /dados é o único que existe.
RUN mkdir -p /dados/cache/npm /dados/cache/uv /dados/cache/python \
 && chown -R 65532:65532 /dados

# UID numérico, não o nome "patchbay": é o que o Kubernetes com runAsNonRoot
# precisa para provar que o processo não é root.
USER 65532:65532

EXPOSE 8787

# PATCHBAY_DATA_DIR é onde o SQLite grava patchbay.db.
VOLUME ["/dados"]
ENV PATCHBAY_DATA_DIR=/dados
ENV PATCHBAY_LISTEN=0.0.0.0:8787

# Onde npx e uvx escrevem. Todos apontam para dentro de /dados porque ele é o
# único ponto gravável com rootfs somente leitura — e porque cache em volume
# significa que reiniciar o patchbay não rebaixa todo pacote de novo. HOME entra
# junto: sem ele o npm tenta /nonexistent e falha antes de olhar o cache.
ENV HOME=/dados
ENV npm_config_cache=/dados/cache/npm
ENV npm_config_update_notifier=false
ENV UV_CACHE_DIR=/dados/cache/uv
ENV UV_PYTHON_INSTALL_DIR=/dados/cache/python

# Forma exec: PID 1 recebe SIGTERM diretamente. O gateway trata o sinal
# desligando as sessões MCP e o servidor HTTP antes de sair.
#
# Sem init/tini mesmo com filhos: quem cria processo aqui é o supervisor de
# internal/platform/stdioproc, que já usa process group e mata a árvore inteira
# no encerramento — foi escrito exatamente para o neto que `npx` deixa vivo.
ENTRYPOINT ["/patchbay"]
CMD ["serve"]

# Sem HEALTHCHECK: em Docker Compose, use um serviço externo apontando o gateway
# MCP para /mcp/{slug}; no Kubernetes, a probe é HTTP direta contra o mesmo
# caminho — que o Kubernetes ignora HEALTHCHECK de Dockerfile de qualquer forma.
