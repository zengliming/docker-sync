# syntax=docker/dockerfile:1
#
# docker-sync 多阶段构建：
#   构建阶段 golang:1.25-alpine 编译静态二进制（CGO 关闭，便于在 alpine 中运行）
#   运行阶段 alpine:3.22，带 ca-certificates（访问 HTTPS registry）和 tzdata（cron 本地时区）
#
# 注意：运行镜像中不包含 docker CLI，也不挂载 /var/run/docker.sock。
#      服务直接通过 Registry v2 HTTP API 拉取 manifest/blob，并自行生成
#      `docker save` 兼容的 tar；用户把 tar 下载到宿主机后再 `docker load -i`。

ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.22
ARG VERSION=0.1.0

# ---------- 构建阶段 ----------
FROM golang:${GO_VERSION}-alpine AS build

# 版本号会注入到 main.version（见 cmd/server/main.go）。
ARG VERSION
ENV CGO_ENABLED=0

WORKDIR /src

# 先只复制依赖清单，充分利用 Docker 层缓存：依赖不变时无需重新下载。
COPY go.mod go.sum ./
RUN go mod download

# 再复制源码（.dockerignore 已排除 data/、bin/、.git 等无关内容）。
COPY . .

# 静态编译，产物名与 module 名保持一致：docker-sync。
# -trimpath 去掉本机路径；-s -w 去掉符号表和调试信息，减小体积。
RUN go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/docker-sync \
        ./cmd/server

# ---------- 运行阶段 ----------
FROM alpine:${ALPINE_VERSION} AS runtime

# ca-certificates：访问 https://registry-1.docker.io、ghcr.io 等必须；
# tzdata：cron 表达式按服务器本地时区解释，需要时区数据库；
# wget（busybox 自带）：用于 HEALTHCHECK。
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 docker-sync \
    && adduser -S -u 10001 -G docker-sync -H -s /sbin/nologin docker-sync \
    && mkdir -p /app /data \
    && chown docker-sync:docker-sync /app /data

WORKDIR /app

# --chown 让非 root 用户可以执行；二进制名保持 docker-sync。
COPY --from=build --chown=docker-sync:docker-sync /out/docker-sync /app/docker-sync

# 数据目录：state.json、blobs/（CAS 缓存）、tars/（生成的 tar）。
# 用命名卷或绑定挂载覆盖时，请确保该目录对 UID 10001 可写。
VOLUME ["/data"]

EXPOSE 8080

USER docker-sync

STOPSIGNAL SIGTERM

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/api/health || exit 1

ENTRYPOINT ["./docker-sync"]
CMD ["-addr", ":8080", "-data", "/data"]
