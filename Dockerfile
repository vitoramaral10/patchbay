# syntax=docker/dockerfile:1

# Imagem do patchbay — gateway MCP self-hosted, binário único.
#
# CGO_ENABLED=0: o driver SQLite (modernc.org/sqlite) é puro Go, então nada
# aqui precisa de gcc nem de glibc — é o que permite terminar em distroless
# "static", a base mais enxuta que existe para Go.
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

# Diretório de dados vazio, criado aqui só para o estágio final poder copiá-lo
# com o dono certo: a distroless não tem shell para um `mkdir` lá.
RUN mkdir -p /dados-vazio

# Imagem final: só o binário, CA certificates, tzdata e o usuário "nonroot"
# (UID 65532) que a distroless já traz — nenhum shell, nenhum gerenciador de
# pacotes, nenhum "curl" sobrevive até aqui.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime

COPY --from=builder --chown=root:root /out/patchbay /patchbay

# /dados precisa EXISTIR na imagem, com dono 65532 — o VOLUME lá embaixo só o
# declara, não o cria. Volume nomeado montado sobre um caminho ausente da
# imagem nasce root:root 0755, e o processo não-root não consegue criar
# patchbay.db ali: o gateway morre em laço com "unable to open database
# file (14)" (SQLITE_CANTOPEN). Existindo o caminho na imagem, o Docker
# replica dono e modo dele ao inicializar o volume enquanto este está vazio —
# o que conserta também um volume já criado antes desta correção.
# Bind mount não passa por aqui: ali quem manda é o dono do diretório no host,
# que o operador precisa ajustar para 65532 (ver docker-compose.yml).
COPY --from=builder --chown=65532:65532 /dados-vazio /dados

# UID numérico, não o nome "nonroot": é o que o Kubernetes com
# runAsNonRoot precisa para provar que o processo não é root.
USER 65532:65532

# Escuta padrão de PATCHBAY_LISTEN (127.0.0.1:8787 é só o default de
# desenvolvimento; em container o operador publica em 0.0.0.0:8787 via
# PATCHBAY_LISTEN, ver docker-compose.yml).
EXPOSE 8787

# PATCHBAY_DATA_DIR é onde o SQLite grava patchbay.db — o único diretório que
# o processo precisa escrever, e por isso o único que sai da rootfs somente
# leitura no compose de exemplo.
VOLUME ["/dados"]
ENV PATCHBAY_DATA_DIR=/dados
ENV PATCHBAY_LISTEN=0.0.0.0:8787

# Forma exec: PID 1 recebe SIGTERM diretamente, sem passar por um /bin/sh que
# a distroless nem tem. O gateway trata o sinal desligando as sessões MCP e o
# servidor HTTP antes de sair.
ENTRYPOINT ["/patchbay"]
CMD ["serve"]

# Sem HEALTHCHECK: a distroless não tem shell nem curl para escrevê-lo, e
# empacotar um binário de healthcheck só para isso é peso sem benefício aqui.
# Em Docker Compose, use um serviço externo apontando o gateway MCP para
# /mcp/{slug} (o handler responde a qualquer requisição MCP válida); no
# Kubernetes, a liveness/readiness probe é HTTP direta contra o mesmo caminho
# — nenhum dos dois orquestradores precisa de HEALTHCHECK do Dockerfile, que
# o Kubernetes ignora de qualquer forma.
