# docker-sync HTTP API 契约

本文件是前后端之间唯一的接口契约。前端只调用 `/api/*` 下的接口，后端负责所有业务逻辑。
所有请求/响应均为 `application/json; charset=utf-8`，仅 tar 下载除外。

## 通用约定

- 成功：HTTP 200/201/202，返回 JSON。
- 失败：HTTP 4xx/5xx，返回 `{"error":"人类可读的错误信息"}`。
- 时间：RFC3339（Go `time.Time` 的默认 JSON 编码），例如 `2026-10-24T10:35:22+08:00`。
- 任务 ID 形如 `t_1a2b3c4d`，运行记录 ID 形如 `r_9f8e7d6c`。
- SSE：`GET /api/events` 返回 `text/event-stream`，事件名为 `update`，`data` 为
  `{"type":"task|run|file|settings","id":"可选","message":"可选","time":"..."}`。
  `type` 表示哪类数据变了，前端收到后重新拉取对应列表即可；每 25 秒会收到注释心跳 `: ping`。
  前端必须容忍 SSE 断开并自动重连（EventSource 默认行为）。

## 健康检查

`GET /api/health` → 200

```json
{"status":"ok","version":"1.0.0","uptimeSec":3721}
```

`GET /api/overview` → 200（仪表盘汇总）

```json
{
  "version": "1.0.0",
  "uptimeSec": 3721,
  "dataDir": "/data",
  "taskCount": 3,
  "enabledCount": 2,
  "runningCount": 1,
  "errorCount": 0,
  "tarCount": 5,
  "tarBytes": 123456789,
  "blobCount": 17,
  "blobBytes": 98765432,
  "recentRuns": [Run...]
}
```

`recentRuns` 为最近 8 条运行记录（不含 `logs`）。

## 任务

### Task 对象

```json
{
  "id": "t_1a2b3c4d",
  "name": "alpine 3.20",
  "image": "docker.io/library/alpine:3.20",
  "platform": "linux/amd64",
  "scheduleType": "interval",
  "intervalMinutes": 60,
  "cron": "",
  "autoSync": true,
  "ignoreLatest": false,
  "keepVersions": 3,
  "enabled": true,
  "status": "idle",
  "lastError": "",
  "lastCheckedAt": "2026-10-24T09:00:00+08:00",
  "lastSyncedAt": "2026-10-24T09:00:02+08:00",
  "lastDigest": "sha256:…",
  "lastConfigDigest": "sha256:…",
  "updateAvailable": false,
  "remoteDigest": "",
  "latestTar": "library_alpine_3.20_linux_amd64_1a2b3c4d5e6f.tar",
  "latestTarSize": 3735552,
  "nextRunAt": "2026-10-24T10:00:00+08:00",
  "lastRunId": "r_9f8e7d6c",
  "createdAt": "2026-10-23T22:00:00+08:00",
  "updatedAt": "2026-10-24T09:00:02+08:00"
}
```

字段说明：

| 字段 | 说明 |
| --- | --- |
| `image` | 完整镜像引用。允许 `alpine:3.20`、`ghcr.io/owner/app:v1`、`busybox@sha256:…` |
| `platform` | `os/arch[/variant]`，空值由后端填默认平台 |
| `scheduleType` | `interval` 或 `cron` |
| `intervalMinutes` | interval 模式下间隔分钟数，最小 1 |
| `cron` | cron 模式下的 5 段标准 cron（分 时 日 月 周），服务器本地时区 |
| `autoSync` | 定时检查发现新 digest 时是否自动 pull 并生成 tar；false 时只记录 `updateAvailable` |
| `ignoreLatest` | true 时，镜像 tag 为 `latest`（含不写 tag 的写法）的任务不参与定时检查与自动同步：不进调度（`nextRunAt` 为空）、不产生定时运行记录；手动「立即同步」「检查更新」不受影响。tag 改成具体版本或 digest 后自动恢复调度 |
| `keepVersions` | 该任务保留的 tar 份数（1–50），旧文件自动清理 |
| `status` | `idle` / `queued` / `running` / `error` / `paused` |
| `lastDigest` | 上一次成功同步的平台 manifest digest |
| `lastConfigDigest` | 上一次成功同步的 image config digest |
| `updateAvailable` / `remoteDigest` | 最近一次检查发现远端 digest 与 `lastDigest` 不同时置位 |
| `latestTar` / `latestTarSize` | 最新生成的 tar 文件名（位于数据目录 tars/ 下）与字节数 |
| `nextRunAt` | 只读，调度器计算的下次检查时间 |
| `lastError` | 最近一次失败原因（成功同步后清空） |

### TaskInput（创建/更新入参）

```json
{
  "name": "alpine 3.20",
  "image": "alpine:3.20",
  "platform": "linux/amd64",
  "scheduleType": "interval",
  "intervalMinutes": 60,
  "cron": "",
  "autoSync": true,
  "ignoreLatest": false,
  "keepVersions": 3,
  "enabled": true
}
```

校验规则：

- `image` 必填，必须可解析（后端 `registry.Parse`）。
- `platform` 可空 → 用 `settings.defaultPlatform`；非法值返回 400。
- `scheduleType` 为空时默认 `interval`；`intervalMinutes` <= 0 时默认 60。
- `scheduleType=cron` 时 `cron` 必填且必须能被 5 段 cron 解析，否则 400。
- `keepVersions` <= 0 时默认 3，上限 50。
- 返回 400 时 `error` 字段说明具体字段错误。

### 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/tasks` | → `{"tasks":[Task...]}`，按创建时间倒序 |
| POST | `/api/tasks` | body=TaskInput → 201 `Task` |
| GET | `/api/tasks/{id}` | → `Task`；不存在 404 |
| PUT | `/api/tasks/{id}` | body=TaskInput → 200 `Task`（整体替换可编辑字段，保留运行状态字段） |
| DELETE | `/api/tasks/{id}` | → `{"ok":true}`，同时删除该任务产生的 tar 文件 |
| POST | `/api/tasks/{id}/sync` | body 可空或 `{"force":false}` → 202 `{"run":Run}`；任务不存在 404，已有运行中任务 409 |
| POST | `/api/tasks/{id}/check` | 同步检查远端是否有更新 → 200 `{"updateAvailable":bool,"remoteDigest":"…","currentDigest":"…","image":"…","platform":"…"}`，失败 502 |
| GET | `/api/tasks/{id}/runs?limit=50` | → `{"runs":[Run...]}`（不含 `logs`），limit 默认 50，最大 200 |

`POST /api/tasks/{id}/sync` 语义：

- `force=false`（默认）：先解析远端 digest，若与 `lastDigest` 相同且最新 tar 仍存在，则直接返回一条
  `status="skipped"` 且已结束的 run（`updateAvailable=false`），不重复下载。
- `force=true`：无论 digest 是否变化都重新下载并生成 tar。
- 接口立即返回 202，前端通过轮询 `GET /api/runs/{id}` 或 SSE 跟踪进度。

## 运行记录

### Run 对象

```json
{
  "id": "r_9f8e7d6c",
  "taskId": "t_1a2b3c4d",
  "taskName": "alpine 3.20",
  "image": "docker.io/library/alpine:3.20",
  "platform": "linux/amd64",
  "trigger": "manual",
  "status": "success",
  "phase": "done",
  "startedAt": "2026-10-24T09:00:00+08:00",
  "finishedAt": "2026-10-24T09:00:03+08:00",
  "updateAvailable": true,
  "remoteDigest": "sha256:…",
  "previousDigest": "sha256:…",
  "configDigest": "sha256:…",
  "tarName": "library_alpine_3.20_linux_amd64_1a2b3c4d5e6f.tar",
  "tarSize": 3735552,
  "layers": 4,
  "bytesDownloaded": 1234567,
  "blobsReused": 30,
  "error": "",
  "logs": [
    {"time":"2026-10-24T09:00:00+08:00","level":"info","message":"开始解析镜像 docker.io/library/alpine:3.20 (linux/amd64)"}
  ]
}
```

- `trigger`：`manual`（手动同步）/ `schedule`（定时检查）/ `check`（手动检查）/ `upload`（手动上传 SMB）。
- `status`：`running` / `success` / `skipped` / `error` / `canceled`。
- `phase`：`queued` / `resolving` / `downloading` / `packing` / `done`，用于展示进度。
- `logs` 仅在单个 run 详情接口里返回，最多保留最近 500 行。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/runs?limit=50` | → `{"runs":[Run...]}`，全部任务，按开始时间倒序 |
| GET | `/api/runs/{id}` | → `Run`（含 logs） |

## 文件（tar 产物）

`GET /api/files` → `{"files":[FileItem...]}`

```json
{
  "name": "library_alpine_3.20_linux_amd64_1a2b3c4d5e6f.tar",
  "size": 3735552,
  "modTime": "2026-10-24T09:00:03+08:00",
  "taskId": "t_1a2b3c4d",
  "taskName": "alpine 3.20",
  "image": "docker.io/library/alpine:3.20",
  "digest": "sha256:…",
  "url": "/api/files/library_alpine_3.20_linux_amd64_1a2b3c4d5e6f.tar"
}
```

- `GET /api/files/{name}` → 200，`Content-Type: application/x-tar`，
  `Content-Disposition: attachment; filename="<name>"`，文件不存在 404，非法文件名 400。
- `DELETE /api/files/{name}` → `{"ok":true}`，非法/不存在 404/400。
- `POST /api/files/{name}/smb` → 手动把该 tar 复制到已配置的 SMB 共享，无请求体：
  - 200 `{"ok":true,"file":FileItem,"run":Run}`（同步执行，耗时随文件大小与网络变化）；
  - 400 文件名非法，或 SMB 未启用/配置无效；
  - 404 文件不存在；
  - 502 `{"error":"…","run":Run}` 上传失败（含原因，本地 tar 保留）；
  - 每次调用都会写入一条运行记录（`trigger: "upload"`，`phase: "uploading"` → `done`），因此手动补传同样出现在运行记录列表与 SSE 事件流中。

## 设置

`GET /api/settings` → 200

```json
{
  "defaultPlatform": "linux/amd64",
  "maxConcurrentRuns": 2,
  "maxConcurrentDownloads": 4,
  "blobCacheMaxGB": 20,
  "blobCacheMaxAgeDays": 7,
  "proxyUrl": "",
  "noProxy": "",
  "mirror": {
    "enabled": false,
    "allowAll": false,
    "upstreams": ["docker.io"],
    "tagTtlMinutes": 60
  },
  "registries": [
    {"host":"docker.io","username":"","password":"","hasPassword":false,"insecure":false,"proxyUrl":""},
    {"host":"registry.local:5000","username":"admin","password":"","hasPassword":true,"insecure":true,"proxyUrl":""}
  ],
  "dataDir": "/data"
}
```

- GET 永远不返回明文密码，只返回 `hasPassword`。
- `PUT /api/settings` 接受同样结构；`password` 为空且 `hasPassword=true` 表示保留原有密码；
  传 `password` 会覆盖，`hasPassword=false` 且密码为空表示清空。`dataDir` 只读，忽略入参。
- 校验：并发数 1–16，blobCacheMaxGB 1–1024，blobCacheMaxAgeDays 1–365，
  `defaultPlatform` 合法，registries 的 host 非空且不重复，
  `proxyUrl` / `registries[].proxyUrl` 必须是合法代理地址，`noProxy` 条目只能是主机名，
  `mirror.upstreams` 只能是主机名（可带端口）且不重复，`mirror.tagTtlMinutes` 0–1440
  （0 表示使用默认值 60）。

### 代理字段

| 字段 | 作用域 | 语义 |
| --- | --- | --- |
| `proxyUrl` | 全局 | 所有出站请求使用的代理。空 = 跟随环境变量 `HTTP_PROXY`/`HTTPS_PROXY`；`-` = 强制直连 |
| `noProxy` | 全局 | 逗号分隔的直连列表，支持 `docker.io`、`host:port`、`*.corp.local`、`*`；命中后跳过代理 |
| `registries[].proxyUrl` | 单个仓库 | 空 = 继承全局；`-` = 该仓库强制直连；其他值 = 覆盖全局代理 |

- 支持的协议：`http`、`https`、`socks5`、`socks5h`、`socks4`（`socks5h` 与 `socks5` 均由 Go 解析为远程 DNS）。
- 地址省略协议时默认补 `http://`，因此 `127.0.0.1:7890` 与 `http://127.0.0.1:7890` 等价。
- 该代理作用于所有 Registry 出站请求：manifest、blob 下载、token 换票。
- 修改保存在 `PUT /api/settings` 提交后立即生效（registry 客户端缓存会被清空）。

### 测试代理连通性

`POST /api/settings/proxy-test`

```json
{"proxyUrl":"http://127.0.0.1:7890","noProxy":"docker.io","image":"alpine:3.20"}
```

- 三个字段都可选：`proxyUrl`/`noProxy` 省略时取已保存的配置；`image` 省略时探测 Docker Hub
  （`https://registry-1.docker.io/v2/`），传入镜像引用时探测该镜像所属仓库，并尊重该仓库的 `insecure` 设置。
- → 200

```json
{"ok":true,"target":"https://registry-1.docker.io/v2/","proxyUrl":"http://127.0.0.1:7890","noProxy":"docker.io","elapsedMs":42}
```

- 探不到时仍是 200，但 `ok:false` 且带 `error` 字段（如连接失败、代理认证失败 407）。
- 入参非法（代理地址不合法、noProxy 含协议或路径、镜像引用无法解析）返回 400。

### 镜像加速字段

| 字段 | 语义 |
| --- | --- |
| `enabled` | 打开后本服务的 `/v2/` 变成只读 pull-through 缓存，docker 客户端可直接拉取 |
| `allowAll` | `true` = 允许代理任意上游（按请求路径首段识别主机名）；`false` = 只服务 `upstreams` 中列出的上游，首段未匹配的名称整体作为默认上游的仓库名 |
| `upstreams` | 允许的上游主机名列表，**第一条是默认上游**；空数组按 `["docker.io"]` 处理 |
| `tagTtlMinutes` | tag → digest 映射的缓存时长（0–1440，0 表示默认 60）；过期后重新向上游确认 tag 是否移动 |

- 保存设置后立即生效，同时清空 tag 缓存（blob 与 manifest 缓存保留）。
- 镜像加速与同步任务共用同一份 blob 缓存，因此「blob 缓存上限 / 过期天数」对它同样生效。

### 镜像加速统计

`GET /api/mirror/stats` → 200

```json
{
  "enabled": true,
  "allowAll": false,
  "upstreams": ["docker.io"],
  "tagTtlMinutes": 60,
  "localAddresses": ["192.168.1.10:8080"],
  "scheme": "https",
  "tls": {
    "enabled": true,
    "mode": "self-signed",
    "sans": ["127.0.0.1", "192.168.1.10", "localhost", "mirror.corp.local"],
    "notAfter": "2029-01-28T14:09:49Z",
    "caUrl": "/api/mirror/ca.crt"
  },
  "stats": {"manifestHits": 12, "manifestMisses": 3, "blobHits": 40, "blobMisses": 8, "bytesServed": 12345678}
}
```

`localAddresses` 是给 docker 客户端用的候选地址（前端「客户端用法」直接读它）：优先返回浏览器实际访问
的地址；当该地址是 `localhost`/`127.0.0.1` 等回环名时，改为返回本机非回环 IPv4 地址（端口沿用当前
服务端口），因为 `localhost` 只在服务本机上有效。前端在拿到该字段后会用它替换掉首屏的 `localhost`。

`scheme` 是当前监听协议（`http` / `https`），前端据此生成客户端命令；`tls` 块在明文部署时是
`{"enabled": false}`，HTTPS 部署时给出模式、证书覆盖的地址（`sans`）、到期时间（`notAfter`）
以及 CA 下载地址（`caUrl`，仅内置自签证书有；外部证书不提供）。

### 下载内置 CA

`GET /api/mirror/ca.crt` → 200 `application/x-pem-file`，响应体是 PEM 格式的 CA 证书。

- 供 docker 客户端安装：放到 `/etc/docker/certs.d/<host:port>/ca.crt`（Docker Desktop 用
  `~/.docker/certs.d/<host:port>/ca.crt`），详见 README「用内置自签证书走 HTTPS」。
- `TLS_MODE=off`（明文）或 `TLS_MODE=files`（外部证书）时返回 404 + `{"error": "..."}`。
- 不参与鉴权（本服务无鉴权），但只暴露公钥证书，不含私钥。


## 镜像加速站点（Registry v2）

docker 客户端把本服务当作镜像仓库访问，接口遵循 Registry v2：

| 请求 | 行为 |
| --- | --- |
| `GET /v2/` | 200 `{}`，带 `Docker-Distribution-Api-Version: registry/2.0`；未启用时 404 |
| `GET`/`HEAD /v2/<name>/manifests/<tag\|digest>` | 原样透传上游 manifest（包括 image index，不改写平台），带 `Docker-Content-Digest` |
| `GET`/`HEAD /v2/<name>/blobs/<digest>` | 命中缓存直接下发并支持 `Range`；未命中时边下边写缓存；HEAD 只回长度不回源全量 |
| 其他方法 | `POST`/`PUT`/`PATCH` 一律 405（只读缓存，不支持 push） |
| 其他路径 | `/v2/<name>/tags/list`、`/v2/<name>/blobs/uploads/` 等返回 404/405 |

- `<name>` 的首段命中 `mirror.upstreams` 时按该上游解析，否则整体作为默认上游的仓库名：
  `library/alpine` → `docker.io/library/alpine`，`ghcr.io/owner/app`（`ghcr.io` 在白名单内）→ 上游 `ghcr.io`。
- 错误体遵循 Registry 规范：`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"..."}]}`，状态码 400/404/405/502。
- `/v2/` 路径不经过 gzip 中间件，响应带精确的 `Content-Length`，`Range` 与条件请求原样支持。

## 静态页面

- `GET /` → `index.html`
- `GET /static/*` → 前端静态资源（后端从二进制内嵌的 web 目录读取）
- 未知路径返回 404（`/api/*` 返回 JSON 错误，其他路径返回 index.html）。

## 错误码约定

| 状态码 | 场景 |
| --- | --- |
| 400 | 入参校验失败、非法文件名 |
| 404 | 任务/运行记录/文件不存在 |
| 409 | 任务已有正在运行的同步 |
| 502 | 访问镜像仓库失败（网络、鉴权、digest 校验等） |
| 500 | 服务内部错误 |


## SMB 远端存储

在「设置 → SMB 远端存储」启用自动上传，填写共享地址（例如 `//192.168.1.10/backups`）、共享内相对目录（例如 `docker-images`）、用户名、密码和可选域。目标目录须预先创建，并赋予账号写入、重命名及删除临时文件的权限。使用 SMB2/SMB3，无需挂载共享或授予容器特权。

容器镜像不需要安装 `samba-client`：上传使用内置的纯 Go SMB2/SMB3 客户端，只要求容器能出站访问共享主机的 445 端口；直接运行二进制时同样无需额外依赖。

tar 在本地生成并校验后，进入 `uploading` 阶段；上传到唯一 `.part` 临时文件，完成后重命名为原 tar 文件名，同名远端文件会替换。失败会记录运行错误并保留本地 tar，下次手动或定时同步重试；镜像 digest 不变时也会重新上传已有 tar。仅「检查更新」不会上传文件。启用前已有文件可在「文件」页对单个 tar 点击「上传 SMB」手动补传（`POST /api/files/{name}/smb`）。

远端副本不随本地版本清理而删除，请在 NAS 上单独设置保留策略。密码与仓库凭据一样保存在数据目录的 `state.json`，读取设置接口不返回密码。更换共享地址、用户名或域后需重新填写密码。

`GET/PUT /api/settings` 新增 `smb` 对象（默认禁用）。PUT 时与其他设置一起提交：

```json
{
  "smb": {
    "enabled": true,
    "share": "//192.168.1.10/backups",
    "directory": "docker-images",
    "username": "backup",
    "password": "your-password",
    "hasPassword": false,
    "domain": ""
  }
}
```

GET 隐藏 `password`，通过 `hasPassword` 标记是否已设置。空密码且 `hasPassword: true` 仅在共享、用户名、域均未改变时保留密码；`hasPassword: false` 配合空密码可清除密码。目录必须为相对路径，不允许 `..`、反斜杠、引号或控制字符。上传失败时运行状态为 `error`，本地文件保留。
