# docker-sync 多阶段构建：
#   构建阶段 golang:1.25-alpine 编译静态二进制（CGO 关闭，便于在 alpine 中运行）
#   运行阶段 alpine:3.22，带 ca-certificates（访问 HTTPS registry）和 tzdata（cron 本地时区）
#
# 刻意不使用 `# syntax=docker/dockerfile:1`：该指令会让 BuildKit 先去 Docker Hub
# 拉取 docker/dockerfile frontend 镜像，在无法直连 Docker Hub 的网络里会直接
# `failed to resolve source metadata for docker.io/docker/dockerfile:1`。
# 本文件使用 BuildKit 内置 Dockerfile frontend，无需额外下载。
#
# 基础镜像与依赖源都可通过 build-arg 指向镜像站，例如：
#   docker build \
#     --build-arg GO_IMAGE=docker.m.daocloud.io/library/golang:1.25-alpine \
#     --build-arg RUNTIME_IMAGE=docker.m.daocloud.io/library/alpine:3.22 \
#     --build-arg APK_MIRROR=https://mirrors.aliyun.com/alpine \
#     -t docker-sync:local .
#
# 注意：运行镜像中不包含 docker CLI，也不挂载 /var/run/docker.sock。
#      服务直接通过 Registry v2 HTTP API 拉取 manifest/blob，并自行生成
#      `docker save` 兼容的 tar；用户把 tar 下载到宿主机后再 `docker load -i`。

ARG GO_IMAGE=docker.io/library/golang:1.25-alpine
ARG RUNTIME_IMAGE=docker.io/library/alpine:3.22
ARG VERSION=0.1.0

# ---------- 构建阶段 ----------
FROM ${GO_IMAGE} AS build

# 版本号会注入到 main.version（见 cmd/server/main.go）。
ARG VERSION
# GOPROXY 可换成 https://goproxy.cn,direct 等国内代理。
ARG GOPROXY=https://proxy.golang.org,direct
ENV CGO_ENABLED=0 \
    GOPROXY=${GOPROXY}

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
FROM ${RUNTIME_IMAGE} AS runtime

# ca-certificates：访问 https://registry-1.docker.io、ghcr.io 等必须；
# tzdata：cron 表达式按服务器本地时区解释，需要时区数据库；
# wget（busybox 自带）：用于 HEALTHCHECK。
# APK_MIRROR 留空走默认源；走镜像站时按 build-arg 传入，例如 https://mirrors.aliyun.com/alpine。
ARG APK_MIRROR=
RUN set -eux; \
    if [ -n "$APK_MIRROR" ]; then \
        printf '%s/v%s/main\n%s/v%s/community\n' "$APK_MIRROR" "$(cut -d. -f1,2 /etc/alpine-release)" "$APK_MIRROR" "$(cut -d. -f1,2 /etc/alpine-release)" > /etc/apk/repositories; \
    fi; \
    apk add --no-cache ca-certificates tzdata \
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
