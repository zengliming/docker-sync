# 部署指南

本文覆盖 docker-sync 的容器化部署：构建、运行、权限、反向代理、备份升级与故障排查。
项目结构与功能说明见 [README](../README.md)，HTTP 接口见 [API.md](API.md)。

> **验证说明**：docker-sync 的运行不依赖 Docker daemon 或 `docker.sock`，但**构建镜像**需要 Docker Engine
> （或 BuildKit/`docker buildx`）。本仓库的 Dockerfile / compose 文件经过语法与路径复核，
> 开发环境没有 Docker CLI，因此未做过实际 `docker build`；首次部署时请留意构建日志。

## 1. 前置条件

| 项目 | 要求 |
| --- | --- |
| Docker Engine | 20.10+（含 `docker compose` v2 插件）；只需要用来构建/运行本服务 |
| 网络 | 容器需能出站访问镜像仓库的 HTTPS（`443`）与 token 服务 |
| 磁盘 | 预估 = tar 总量（受 `keepVersions` 约束）+ blob 缓存（默认上限 20 GB） |
| 端口 | 默认 `8080/tcp`，无鉴权，不要直接暴露到公网 |

docker-sync 自身**不需要** docker CLI、`docker.sock`、特权模式或 `--network host`。

## 2. 构建镜像

```bash
cd docker-sync

# 直接构建
docker build -t docker-sync:local .

# 或指定版本号（写入二进制 main.version，界面上可见）
docker build --build-arg VERSION=0.1.0 -t docker-sync:0.1.0 .

# 交叉构建其他架构（一次一个平台，--load 到本地镜像库）
docker buildx build --platform linux/arm64 -t docker-sync:arm64 --load .

# 如需一次产出多架构 manifest，必须推送到 registry（--push），本地无法 --load 多平台清单
docker buildx build --platform linux/amd64,linux/arm64 \
  -t registry.example.com/library/docker-sync:0.1.0 --push .
```

Dockerfile 采用两阶段构建：

- **build**：`golang:1.25-alpine`，`CGO_ENABLED=0`，先 `COPY go.mod go.sum` 再 `go mod download` 以复用依赖层，
  最后 `go build -trimpath -ldflags="-s -w -X main.version=..." -o /out/docker-sync ./cmd/server`；
- **runtime**：`alpine:3.22` + `ca-certificates` + `tzdata`，以非 root 用户 `docker-sync`（UID/GID **10001**）运行，
  二进制位于 `/app/docker-sync`，数据卷 `/data`。

`.dockerignore` 已排除 `data/`、`bin/`、`*.tar`、`.git/`、`node_modules/` 等，构建上下文很小。

### 2.1 构建时无法访问 Docker Hub

`docker build` 需要拉取两个基础镜像（`golang:1.25-alpine`、`alpine:3.22`），并在构建阶段访问
apk 源与 Go module 源。无法直连 Docker Hub 时典型报错：

```
ERROR: failed to resolve source metadata for docker.io/library/golang:1.25-alpine: ... EOF
```

Dockerfile 刻意**不使用** `# syntax=docker/dockerfile:1`（该指令会让 BuildKit 先去拉
`docker.io/docker/dockerfile:1` frontend 镜像，纯内网/受限网络下会直接失败），并把基础镜像、
apk 源、GOPROXY 都做成了 build-arg：

| build-arg | 默认值 | 说明 |
| --- | --- | --- |
| `GO_IMAGE` | `docker.io/library/golang:1.25-alpine` | 构建阶段基础镜像 |
| `RUNTIME_IMAGE` | `docker.io/library/alpine:3.22` | 运行阶段基础镜像 |
| `GOPROXY` | `https://proxy.golang.org,direct` | Go module 代理，国内可换 `https://goproxy.cn,direct` |
| `APK_MIRROR` | 空（官方源） | 运行阶段 apk 源前缀，如 `https://mirrors.aliyun.com/alpine` |
| `VERSION` | `0.1.0` | 注入 `main.version` |

```bash
docker build \
  --build-arg GO_IMAGE=docker.m.daocloud.io/library/golang:1.25-alpine \
  --build-arg RUNTIME_IMAGE=docker.m.daocloud.io/library/alpine:3.22 \
  --build-arg APK_MIRROR=https://mirrors.aliyun.com/alpine \
  --build-arg GOPROXY=https://goproxy.cn,direct \
  -t docker-sync:local .
```

compose 场景用同名环境变量覆盖（`docker-compose.yml` 已把它们接成 `${GO_IMAGE:-...}`）：
`GO_IMAGE=... RUNTIME_IMAGE=... docker compose up -d --build`。

另一种更省事的做法：给 Docker daemon 配 `registry-mirrors`（`/etc/docker/daemon.json`），
之后所有拉取自动走镜像站，构建命令保持原样。

若基础镜像已在本地（`docker images` 能看到 `golang:1.25-alpine` 与 `alpine:3.22`），
不加 `--pull` 时构建会直接复用本地镜像，无需联网。

## 3. 运行容器

### 3.1 docker compose（推荐）

```bash
mkdir -p data
sudo chown -R 10001:10001 data     # Linux 宿主机：让容器内 UID 10001 可写

docker compose up -d --build
docker compose logs -f
```

打开 `http://<host>:8080`。compose 已配置：

- `ports: "8080:8080"`
- `volumes: ./data:/data`
- `restart: unless-stopped`
- `stop_grace_period: 40s`（服务优雅关闭上限 30s，留出余量）

需要在容器内改变监听地址/日志级别/时区时，取消 `docker-compose.yml` 中 `environment` 的注释即可，
或在同目录放一个 `docker-compose.override.yml`（已被 `.dockerignore` 排除，不影响镜像构建）。

### 3.2 docker run

```bash
docker run -d \
  --name docker-sync \
  --restart unless-stopped \
  -p 8080:8080 \
  -v "$PWD/data:/data" \
  -e LOG_LEVEL=info \
  -e TZ=Asia/Shanghai \
  docker-sync:local
```

`-addr` / `-data` / `-log-level` 三个 flag 与 `ADDR` / `DATA_DIR` / `LOG_LEVEL` 环境变量等价，
命令行 flag 优先。容器默认 `-addr :8080 -data /data`，通常不用改。

### 3.3 使用命名卷

不想处理宿主机目录权限时，可把 `/data` 换成命名卷：

```bash
docker run -d --name docker-sync -p 8080:8080 \
  -v docker-sync-data:/data docker-sync:local
```

Docker 会自动把卷的属主设置为镜像中 `/data` 的属主（UID 10001），无需手工 chown。
代价是数据不在宿主机目录里，备份要用 `docker run --rm -v docker-sync-data:/data ... tar` 或
`docker cp`。

## 4. 文件权限（UID 10001）

运行镜像里没有 root：容器进程以 `docker-sync`（UID 10001，无登录 shell）启动。
绑定挂载 `./data` 时，宿主机目录必须对 10001 可写，否则会出现
`open /data/state.json: permission denied` 或无法创建 `blobs/`、`tars/`。

```bash
# 首次部署或换机器后执行
sudo chown -R 10001:10001 data
sudo chmod -R u+rwX data
```

- Linux 上如果宿主机用户不是 10001，也可以改用命名卷（见 3.3），或让目录组可写（`chgrp` + `g+w`）。
- Docker Desktop（Windows/macOS）的绑定挂载一般自动处理权限，无需 chown。
- 不建议用 `--user root` 绕过问题：`state.json` 会被 root 拥有，之后切回非 root 又会失败。

## 5. 反向代理与安全

服务**没有内置鉴权**，任何能访问 8080 的人都可以创建任务、读取 `state.json` 暴露的信息
（私库密码已掩码返回，但明文仍存在文件里）、下载/删除 tar、修改设置。部署原则：

1. 只监听内网地址，例如 `-addr 127.0.0.1:8080` 或 compose 里 `ports: "127.0.0.1:8080:8080"`；
2. 需要远程访问时，放在带认证的反向代理之后，并启用 HTTPS；
3. 不要把 `data/` 放进 Git 仓库或公开共享目录。

### 5.1 Nginx

```nginx
server {
    listen 443 ssl;
    server_name sync.example.com;

    ssl_certificate     /etc/letsencrypt/live/sync.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/sync.example.com/privkey.pem;

    auth_basic           "docker-sync";
    auth_basic_user_file /etc/nginx/.htpasswd;

    location / {
        proxy_pass         http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_set_header   X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;

        # SSE（/api/events）需要关闭缓冲并放宽读超时
        proxy_buffering    off;
        proxy_cache        off;
        proxy_read_timeout 3600s;
    }
}
```

注意：

- tar 下载可能很大，Nginx 默认不限制响应大小，但代理读超时要足够长；
- SSE 必须关闭 `proxy_buffering`，否则前端事件会延迟；
- 如果代理上传大小限制（`client_max_body_size`）设得很小，不影响下载，但会影响以后可能的导入功能。

### 5.2 Caddy

```caddy
sync.example.com {
    basicauth {
        admin $2a$14$<bcrypt-hash>
    }
    reverse_proxy 127.0.0.1:8080 {
        flush_interval -1
    }
}
```

Caddy 自动配置 HTTPS；`flush_interval -1` 用于 SSE 即时刷新。

### 5.3 健康检查

`GET /api/health` 返回 200 即视为健康；镜像内置 `HEALTHCHECK`（`wget` 请求 `/api/health`），
compose/K8s 也可以直接复用该路径做存活探针。就绪探针同样可用 `/api/health`。

### 5.4 出站代理

两种方式可以叠加，优先级从高到低：

1. **页面设置（推荐）**：控制台「设置」→「网络代理」填 `proxyUrl` / `noProxy`，
   或给单个仓库填 `proxyUrl`。只影响 registry 出站请求，保存后立即生效，重启容器不丢
   （存在 `data/state.json` 里）。
2. **环境变量**：给容器加 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`，影响进程内所有出站请求。

容器里访问宿主机上的代理不能写 `127.0.0.1`（那是容器自己）：

| 场景 | 写法 |
| --- | --- |
| Docker Desktop（Windows/macOS） | `http://host.docker.internal:7890` |
| Linux 宿主机 + bridge 网络 | `http://172.17.0.1:7890`（docker0 网关）或宿主机内网 IP |
| 代理在另一台机器 | `http://10.0.0.5:3128` |

```yaml
services:
  docker-sync:
    build: .
    environment:
      - HTTP_PROXY=http://host.docker.internal:7890
      - HTTPS_PROXY=http://host.docker.internal:7890
      - NO_PROXY=localhost,127.0.0.1,registry.local
      - TZ=Asia/Shanghai
```

- `NO_PROXY` 条目只写 host/域名（可带端口、`*.suffix`），不要带协议或路径。
- 页面设置一旦填了值就优先于环境变量；想让某个仓库绕过页面代理，在该仓库行填 `-`。
- 想强制全局直连，`proxyUrl` 填 `-`。
- 常见错误：`Get "https://registry-1.docker.io/v2/": proxyconnect tcp: dial tcp ...` 说明代理不可达；
  代理返回 407 表示需要认证（当前版本不支持 `http://user:pass@host:port` 形式的代理认证，
  请改用无需认证的代理或在代理侧做 IP 白名单）。
- 改完配置建议先用页面上的「测试连通性」验证，再触发同步；同步失败时日志里会记录
  `镜像 <ref>: registry: ...` 的具体网络错误。

### 5.5 镜像加速站点

开启「设置 → 镜像加速」后，本服务在同一个端口上额外提供 Registry v2 只读接口（`/v2/`），
可以直接给内网的 docker 客户端当加速站用。

客户端配置（把 `192.168.1.10:8080` 换成实际地址）：

```json
{
  "registry-mirrors": ["http://192.168.1.10:8080"],
  "insecure-registries": ["192.168.1.10:8080"]
}
```

- `registry-mirrors` 只对 Docker Hub 生效；要加速 `ghcr.io`、`quay.io` 等其他上游，
  改成显式拉取：`docker pull 192.168.1.10:8080/ghcr.io/owner/app:v1`（需把 `ghcr.io` 加进上游白名单）。
- 本服务不带 TLS，必须登记 `insecure-registries`。想避免这一项，在前面挂一层带证书的反向代理
  （见 5.1 Nginx / 5.2 Caddy），再把加速地址写成 `https://mirror.example.com`。
- 反向代理需要放开大文件与长连接，建议 `proxy_request_buffering off;`、`client_max_body_size 0;`，
  并保留 `Range` 头（Nginx 默认透传）。
- 白名单：`allowAll` 关闭时只代理列表里的上游（默认 `docker.io`），避免成为开放代理；
  内网使用时建议保持关闭。
- 存储：加速缓存写入同一个数据目录的 blob 缓存，受「blob 缓存上限 / 过期天数」约束，
  定期清理可回收空间；存量 tar 不受影响。
- 排障：
  - `docker pull` 报 `http: server gave HTTP response to HTTPS client` → 没加 `insecure-registries`。
  - 报 `manifest unknown` → 上游白名单里没有该 host，或上游确实没有这个 tag。
  - 报 429 / `toomanyrequests` → 上游限流，给对应 host 配置「仓库凭据」，或改用内网镜像站做上游。

## 6. 数据备份与升级

### 6.1 备份

需要备份的是整个数据目录，至少包含：

- `state.json`：任务、设置、运行历史、tar 索引；
- `tars/*.tar`：已生成的镜像包（可重新同步生成，但重新拉取耗时耗流量）；
- `blobs/`：CAS 缓存（**可选**，删掉只会导致下次同步重新下载）。

一致性最好的做法是先停服务再复制：

```bash
docker compose stop
sudo tar -C . -czf docker-sync-backup-$(date +%F).tar.gz data
docker compose start
```

运行中也可以复制：`state.json` 是原子替换写入的，单个文件通常自洽；但正在生成的 `.tar` 可能不完整，
建议备份时跳过 `tars/` 中正在写入的文件，或接受「已下载 tar 可能在恢复后索引对账时被清理」。
恢复时把 `data/` 放回原位置、确保 UID 10001 可写，再启动即可（缺失的 tar 索引会自动对账）。

### 6.2 升级

```bash
git pull
docker compose build --pull
docker compose up -d
```

- 数据卷不受影响；启动时若 `state.json` 结构升级，服务会按新版本默认值补齐缺失字段。
- 回滚：用旧 tag 重新 `docker compose up -d`（建议给每次发布打 `docker-sync:<version>` tag）。
- 降级前先备份 `state.json`；旧版本可能不认识新字段。
- 单实例部署即可：同一个 `data/` 目录不要被多个容器同时挂载写入（`state.json` 没有跨进程锁）。

## 7. 故障排查

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| 启动即退出，日志 `permission denied` | 绑定挂载目录属主不是 10001 | `sudo chown -R 10001:10001 data`，或改用命名卷 |
| `bind: address already in use` | 8080 被占用 | 改 `ports`/`-addr`，如 `-p 18080:8080` |
| 页面能开但同步报 TLS/证书错误 | 容器缺 CA，或 registry 使用自签名证书 | 镜像已含公共 `ca-certificates`；自签名证书需自行派生镜像（`USER root` 下 `COPY` CA 到 `/usr/local/share/ca-certificates/` 并 `update-ca-certificates`，再切回 UID 10001）；`insecure` 只切换到 HTTP，不跳过校验 |
| `401 unauthorized` / `denied` | 未配置私有仓库凭据，或 token 权限不足 | 设置页填 host/username/password；确认账号有 pull 权限 |
| Docker Hub 拉取报 `toomanyrequests` | 匿名限流 | 配置 Docker Hub 账号凭据，或改用镜像加速/其他 registry |
| cron 在错误时间执行 | 容器内时区是 UTC | `-e TZ=Asia/Shanghai`（容器已装 `tzdata`） |
| `docker load` 报错 | tar 不完整或传输损坏 | 重新下载；服务发布前会做归档校验，正常不会产出坏包；对比文件大小 |
| 磁盘占用持续增长 | tar 保留过多、blob 缓存上限过高 | 降低任务 `keepVersions`；调小 `blobCacheMaxGB` / `blobCacheMaxAgeDays`；手动清理 `tars/` |
| `state.json` 损坏后任务清空 | 非正常断电/磁盘满 | 原文件被改名为 `state.json.broken`，可人工检查；服务以默认设置启动。恢复后从备份还原 `state.json` |
| 健康检查一直 unhealthy | 监听地址被改成非 8080，或容器内地址不可达 | 健康检查写死 `127.0.0.1:8080`；改端口时同步改 Dockerfile 的 `HEALTHCHECK` 或覆盖它 |
| 大量同步任务排队不执行 | `maxConcurrentRuns` 限制 | 设置页调大（1–16），注意网络与磁盘压力 |

查看日志：

```bash
docker compose logs -f --tail=200
docker inspect --format '{{json .State.Health}}' docker-sync | jq
```

## 8. Kubernetes 提示（可选）

- 用 `Deployment`（`replicas: 1`）+ `PersistentVolumeClaim` 挂载 `/data`；
- 容器以 UID 10001 运行，PVC 需支持 `securityContext.fsGroup: 10001` 或预先 chown；
- 探针用 `httpGet /api/health`（端口 8080）；
- 不要给容器 `docker.sock` 或特权模式，本服务不需要；
- 优雅终止期限建议 ≥ 40s（`terminationGracePeriodSeconds`），让正在收尾的 tar 写完。


## SMB 远端存储

在「设置 → SMB 远端存储」启用自动上传，填写共享地址（例如 `//192.168.1.10/backups`）、共享内相对目录（例如 `docker-images`）、用户名、密码和可选域。目标目录须预先创建，并赋予账号写入、重命名及删除临时文件的权限。使用 SMB2/SMB3，无需挂载共享或授予容器特权。

上传使用内置的纯 Go SMB2/SMB3 客户端，**镜像里不需要 `samba-client`**，也不需要 `--privileged`、`SYS_ADMIN` 或 cifs 挂载能力：

- 网络：容器需能出站访问 SMB 主机的 `445/tcp`。若 NAS 与容器不在同一网段，注意防火墙与 SMB 签名策略；
- 权限：共享内目标目录必须预先创建，账号需具备写入、重命名、删除权限（上传先写 `.part` 再重命名）；
- 用户名密码：`domain` 可留空（本地账号）或填 AD 域名；
- 快速自查：在容器里执行 `nc -vz <nas-host> 445` 应能连通；配置保存后可在「文件」页对任一 tar 点「上传 SMB」验证。

tar 在本地生成并校验后，进入 `uploading` 阶段；上传到唯一 `.part` 临时文件，完成后重命名为原 tar 文件名，同名远端文件会替换。失败会记录运行错误并保留本地 tar，下次手动或定时同步重试；镜像 digest 不变时也会重新上传已有 tar。仅「检查更新」不会上传文件。启用前已有文件可在「文件」页手动补传。

远端副本不随本地版本清理而删除，请在 NAS 上单独设置保留策略。密码与仓库凭据一样保存在数据目录的 `state.json`，读取设置接口不返回密码。更换共享地址、用户名或域后需重新填写密码。
