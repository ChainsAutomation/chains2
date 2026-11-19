FROM node:22-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/index.html web/tsconfig.json web/vite.config.ts web/playwright.config.ts ./
COPY web/src ./src
RUN npm run build

FROM golang:1.25-bookworm AS builder
RUN apt-get update && apt-get install -y --no-install-recommends libmosquitto-dev libsqlite3-dev libevdev-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY bus ./bus
COPY cmd ./cmd
COPY engine ./engine
COPY operator ./operator
COPY rules ./rules
COPY sdk ./sdk
COPY services ./services
COPY state ./state
COPY web/assets*.go ./web/
COPY z2m ./z2m
COPY --from=frontend /src/web/dist ./web/dist
RUN CGO_ENABLED=1 go build -trimpath -buildvcs=false -tags webui -o /out/chains ./cmd/chains \
    && CGO_ENABLED=1 go build -trimpath -buildvcs=false -o /out/chains-go-service ./cmd/chains-go-service \
    && CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/chainsctl ./cmd/chainsctl

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libmosquitto1 libsqlite3-0 libevdev2 tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 chains && useradd --uid 10001 --gid chains --home-dir /var/lib/chains chains \
    && install -d -o chains -g chains /var/lib/chains
COPY --from=builder /out/ /usr/local/bin/
ENV HOME=/var/lib/chains XDG_STATE_HOME=/var/lib/chains XDG_CONFIG_HOME=/etc
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["chains"]
CMD ["engine"]
