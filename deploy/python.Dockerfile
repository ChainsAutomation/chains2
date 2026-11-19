FROM ghcr.io/astral-sh/uv:0.13.9 AS uv

FROM python:3.13-slim-bookworm AS builder
COPY --from=uv /uv /usr/local/bin/uv
WORKDIR /src
ENV UV_PROJECT_ENVIRONMENT=/opt/venv UV_LINK_MODE=copy
COPY python/pyproject.toml python/uv.lock ./
RUN uv sync --locked --no-dev --no-install-project --extra rflink --extra radicale
COPY python/src ./src
RUN uv sync --locked --no-dev --no-editable --extra rflink --extra radicale

FROM python:3.13-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 chains && useradd --uid 10001 --gid chains --home-dir /var/lib/chains chains \
    && install -d -o chains -g chains /var/lib/chains
COPY --from=builder /opt/venv /opt/venv
ENV PATH=/opt/venv/bin:$PATH HOME=/var/lib/chains XDG_STATE_HOME=/var/lib/chains XDG_CONFIG_HOME=/etc
USER 10001:10001
ENTRYPOINT ["chains-service"]
CMD ["--list"]
