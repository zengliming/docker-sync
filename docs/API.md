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

- `trigger`：`manual`（手动同步）/ `schedule`（定时检查）/ `check`（手动检查）。
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

## 设置

`GET /api/settings` → 200

```json
{
  "defaultPlatform": "linux/amd64",
  "maxConcurrentRuns": 2,
  "maxConcurrentDownloads": 4,
  "blobCacheMaxGB": 20,
  "blobCacheMaxAgeDays": 7,
  "registries": [
    {"host":"docker.io","username":"","password":"","hasPassword":false,"insecure":false},
    {"host":"registry.local:5000","username":"admin","password":"","hasPassword":true,"insecure":true}
  ],
  "dataDir": "/data"
}
```

- GET 永远不返回明文密码，只返回 `hasPassword`。
- `PUT /api/settings` 接受同样结构；`password` 为空且 `hasPassword=true` 表示保留原有密码；
  传 `password` 会覆盖，`hasPassword=false` 且密码为空表示清空。`dataDir` 只读，忽略入参。
- 校验：并发数 1–16，blobCacheMaxGB 1–1024，blobCacheMaxAgeDays 1–365，
  `defaultPlatform` 合法，registries 的 host 非空且不重复。

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
