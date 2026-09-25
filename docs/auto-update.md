# 容器自动更新功能

给「检测到有更新的容器」加上无人值守的自动更新能力：定时把有新版本的容器重建一遍，重建完删掉旧镜像，并且支持开关和排除列表。

前端对应代码在同仓库的 `frontend/` 目录，见文末「构建与部署」。

---

## 一、配置持久化

落盘位置：`/data/config/autoUpdate.json`（与前端 logo 配置 `imageLogos.js` 同目录，部署时 `/data` 已挂载卷，容器重建不丢配置）。

可用环境变量 `AUTO_UPDATE_CONFIG` 覆盖路径，方便本地调试：

```bash
AUTO_UPDATE_CONFIG=./autoUpdate.json ./dockercopilot
```

### 字段说明

```json
{
  "enabled": false,
  "deleteOldImage": true,
  "excludeList": ["mysql", "redis*"],
  "protectSelf": true,
  "lastRunAt": "2026-09-17T14:40:12+08:00",
  "lastTrigger": "cron",
  "lastResult": [
    {
      "containerId": "3f2a...",
      "containerName": "jellyfin",
      "image": "jellyfin/jellyfin:latest",
      "success": true,
      "message": "更新成功，旧镜像 jellyfin/jellyfin:10.9.10 已删除",
      "finishedAt": "2026-09-17T14:40:31+08:00"
    }
  ]
}
```

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `enabled` | bool | `false` | 自动更新总开关。**定时任务**受它控制；手动触发（`/run`）无视它 |
| `deleteOldImage` | bool | `true` | 更新完成后是否删除旧镜像 |
| `excludeList` | string[] | `[]` | 排除列表，不参与自动更新的容器 |
| `protectSelf` | bool | `true` | 自身保护，dockerCopilot 容器永不自动更新 |
| `lastRunAt` | string | `""` | 上次执行完成时间（RFC3339） |
| `lastTrigger` | string | `""` | 上次触发来源：`cron` / `manual` |
| `lastResult` | object[] | `[]` | 历史执行结果，最新排在前，最多保留 50 条 |

`lastRunAt` / `lastTrigger` / `lastResult` 是**只读**字段，由服务端写入，接口不接受修改。

### 排除列表的匹配规则

一个条目会依次去匹配三个值，任意一个命中即算排除，**大小写不敏感**：

1. 容器名（`jellyfin`）
2. 镜像全名（`jellyfin/jellyfin:latest`）
3. 镜像仓库名，即去掉 tag（`jellyfin/jellyfin`）

支持 shell 风格通配：`redis*` 匹配 `redis`、`redis-cache`、`redis:7-alpine`；`*/nginx*` 之类也可以。

写入时自动去空格、去重（忽略大小写）、上限 200 条。

### 落盘可靠性

内存缓存 + 读写锁，写盘走「先写 `.tmp` 再 rename」的原子替换，避免写一半掉电把配置写坏。若启动时读到损坏的 JSON，会把它重命名为 `autoUpdate.json.broken` 留档，然后重建默认配置，不会静默丢弃。

---

## 二、接口契约

自动更新相关接口在 `/api/autoUpdate` 下，手动检测接口是 `/api/checkUpdate`（状态查询是 `GET /api/checkUpdate/status`），**均需 JWT**（与其他 `/api/*` 一致，请求头带 `Authorization: Bearer <token>`）。

响应统一是 go-zero 的 `Resp` 信封：

```json
{ "code": 200, "msg": "success", "data": { } }
```

### 1. 读取配置

```
GET /api/autoUpdate
```

`data` 为配置全量字段，另加两个只读辅助字段：

```json
{
  "code": 200,
  "msg": "success",
  "data": {
    "enabled": false,
    "deleteOldImage": true,
    "excludeList": ["mysql", "redis*"],
    "protectSelf": true,
    "lastRunAt": "2026-09-17T14:40:12+08:00",
    "lastTrigger": "cron",
    "lastResult": [],
    "configPath": "/data/config/autoUpdate.json",
    "running": false
  }
}
```

- `configPath` — 配置文件实际路径，排查「配置没生效」时有用
- `running` — 当前是否有一轮自动更新正在执行

### 2. 修改配置

```
PUT /api/autoUpdate
Content-Type: application/json
```

请求体：

```json
{
  "enabled": true,
  "deleteOldImage": true,
  "protectSelf": true,
  "excludeList": ["mysql", "redis*"]
}
```

**注意三点：**

1. `enabled` / `deleteOldImage` / `protectSelf` 是值类型，**每次必须全量回传**。只传 `enabled` 会把另外两个开关写成 `false`。
2. `excludeList` 用「传没传」区分语义：
   - **不带这个字段** → 保持原值不变
   - **传空数组 `[]`** → 用户主动清空排除列表
3. 传非法通配表达式不会报错，该条目退化成精确匹配，不影响其他条目。

响应 `data` 为更新后的配置（不含 `configPath` / `running`）。

### 3. 立即执行一轮

```
POST /api/autoUpdate/run
```

**异步接口**。一轮要更新多台容器，每台都包含一次镜像拉取，可能耗时几分钟，同步等待会把请求挂死。这里立刻返回，前端改为轮询接口 1 的 `running` 与 `lastResult` 获取结果 —— 与现有容器更新的「发起后轮询进度」模式一致。

手动触发**无视总开关**，方便在开关关闭时先试跑一次看效果。

| code | msg | 说明 |
|---|---|---|
| 200 | 已开始执行自动更新，请稍后刷新查看结果 | 已开始 |
| 409 | 上一轮自动更新还在执行中，请稍后再试 | 有任务在跑，未重复触发 |
| 500 | 自动更新调度器未初始化 | 服务端异常 |

### 4. 查询候选容器

```
GET /api/autoUpdate/candidates
```

返回**所有**容器的筛选结论，含被跳过的及原因。前端用它渲染排除列表的勾选项，让用户直接选真实容器，而不是手打容器名。

```json
{
  "code": 200,
  "msg": "success",
  "data": {
    "running": false,
    "willUpdate": 3,
    "candidates": [
      {
        "containerId": "3f2a...",
        "containerName": "jellyfin",
        "image": "jellyfin/jellyfin:latest",
        "imageId": "sha256:...",
        "state": "running",
        "haveUpdate": true,
        "excluded": false,
        "protected": false,
        "willUpdate": true,
        "excludeReason": ""
      },
      {
        "containerId": "8b1c...",
        "containerName": "dockercopilot",
        "image": "0nlylty/dockercopilot:latest",
        "imageId": "sha256:...",
        "state": "running",
        "haveUpdate": true,
        "excluded": false,
        "protected": true,
        "willUpdate": false,
        "excludeReason": "dockerCopilot 自身，永不自动更新"
      }
    ]
  }
}
```

字段含义：

| 字段 | 说明 |
|---|---|
| `haveUpdate` | 该容器所用镜像当前是否检测到有更新（来自 digest 比对缓存） |
| `excluded` | 命中用户配置的排除列表 |
| `protected` | 命中自身保护，**优先级高于排除列表** |
| `willUpdate` | 本轮会被自动更新选中 |
| `excludeReason` | 被跳过的原因；`willUpdate` 为 `true` 时是空串 |

筛选判定按以下顺序，先命中先返回：

1. 没检测到更新 → `未检测到更新`
2. 自身保护开启且是 dockerCopilot 容器 → `dockerCopilot 自身，永不自动更新`
3. 命中排除列表 → `命中排除列表`
4. 以上都不满足 → `willUpdate: true`

### 5. 立即检测一次更新

```
POST /api/checkUpdate
```

定时检测在每小时 :30 执行，这个接口让用户不必等到下一个整点，随时手动刷新一次检测结论。

**同步接口**：请求会等到全部镜像检测完成才返回。一轮是若干个轻量 HEAD 请求，通常几秒到几十秒。前端把这个请求的超时单独放宽到 3 分钟（axios 默认的 10 秒不够）。

请求无参数。

```json
{
  "code": 200,
  "msg": "success",
  "data": {
    "needUpdateCount": 2,
    "total": 47,
    "checkedAt": "2026-09-17T16:40:03+08:00"
  }
}
```

| 字段 | 说明 |
|---|---|
| `needUpdateCount` | 本次检测判定有更新的镜像数 |
| `total` | 本次参与检测的镜像数 |
| `checkedAt` | 本次检测完成时间（RFC3339） |
| `skipped` | `true` 表示这次请求**没等到结论**：已有另一轮检测在跑，且等过了 `CheckRunWaitTimeout`（3 分钟）。此时上面三个字段不可信，前端应转去轮询接口 6 |

| code | msg | 说明 |
|---|---|---|
| 200 | success | 检测完成，结论已写入内存缓存 |
| 200 | 已有检测在进行中，请稍候查看结果 | 撞上并发检测且等待超时，`data.skipped = true` |
| 500 | 获取镜像列表失败: ... | 与 Docker daemon 通信异常 |
| 500 | 镜像更新检测模块未初始化 | 服务端异常 |

调用成功后，`GET /api/containers` 返回的 `haveUpdate` 标记会立即反映最新结论。

**与定时任务撞车时会合流（重要）**：同一时刻只允许一轮检测在跑。若请求到达时正好有一轮在跑
（例如 `:30` 的定时任务、或别人刚点了按钮），本次请求会**等那一轮跑完并直接复用它的结论** ——
不会自己再跑一遍，所以返回的仍然是新鲜数据。只有等到 `CheckRunWaitTimeout`（3 分钟）都没等到时，
才返回 `skipped = true`。三条触发路径（定时任务 / 手动按钮 / 自动更新前的刷新）因此不会叠加成
并发的重复检测。


**超时 ≠ 失败（重要）**：前端等待到上限后断开，只是「客户端不再等」——服务端不受影响，
会继续跑完并把结论写进内存，列表页每 10 秒轮询一次，结果会自己出现。
前端据此走下面的状态接口，而不是弹一个假的「检测失败」。

### 6. 查询检测状态

```
GET /api/checkUpdate/status
```

给「检测耗时超过前端等待上限」这条路径兜底用的：前端超时后转成轮询这个接口，
直到这一轮真正结束，再把真实结论报给用户。

**只读内存快照**：不触发检测、不发任何网络请求、不改动任何数据，可以放心高频轮询。

请求无参数。

```json
{
  "code": 200,
  "msg": "success",
  "data": {
    "running": false,
    "startedAt": "2026-09-17T16:39:12.345678901+08:00",
    "lastCheckedAt": "2026-09-17T16:40:03.987654321+08:00",
    "lastTotal": 47,
    "lastNeedUpdateCount": 2
  }
}
```

| 字段 | 说明 |
|---|---|
| `running` | 当前是否有检测在执行 |
| `startedAt` | 最近一次检测的开始时间；空字符串表示从未检测过 |
| `lastCheckedAt` | 上一轮完成时间；空字符串表示从未检测过 |
| `lastTotal` | 上一轮参与检测的镜像总数 |
| `lastNeedUpdateCount` | 上一轮判定有更新的镜像数 |

时间戳带**纳秒精度**（RFC3339Nano）：调用方是靠「这个值变了没有」判断本轮是否结束的，
秒级精度在同秒内完成的连续两轮里会撞车，导致轮询一直等不到结果。

前端判定「这一轮结束了」用两个条件，满足其一即可：

1. 轮询中亲眼看到 `running` 由 `true` 变 `false`（最可靠）；
2. `lastCheckedAt` 与点击前记录的值不一样（覆盖「这轮刚好在超时前后跑完」的情况）。

用字符串比较而不是比时间大小，可以完全避开浏览器与 NAS 之间的时钟偏差。

此外点击前会先查一次这个接口，若 `running` 为 `true` 就不再发起新检测、直接等结果 ——
顺带避免了重复点击叠加出两轮并发检测。

---

## 三、执行时序与安全约束

### 定时节奏

| 任务 | 时间 | 说明 |
|---|---|---|
| 镜像更新检测 → 自动更新 | 每小时 **:30** | 同一趟任务：先逐个镜像比对远端 digest（结论落内存，供容器列表标记「有更新」），紧接着按配置更新检测到有更新的容器 |

检测与更新**合并成同一趟**，不再错开时间 —— 刚检测出的 digest 就是最新鲜的，更新没必要等到下一个定时点。检测本身总在跑，**是否动容器由总开关决定**：开关关闭时更新阶段立刻返回，检测结论照常写入缓存。此外**进程启动时会立刻检测一次**（启动这趟只检测，不触发更新）。

不想等整点的话，容器页顶部有「检测更新」按钮，可随时手动触发一次检测（见接口 5）—— 它只跑检测阶段，不会顺带更新容器。想立刻更新则用面板里的「立即执行」。

### 检测为什么可能很慢（及已做的优化）

一轮检测对每个镜像要做两次 registry 探测：`GetToken` → `GetChallengeURL` 一次、
`BuildManifestURL` 一次。而 `GetRegistryAddress` 对 docker.io 系镜像要逐个体检候选 host ——
先试官网 `index.docker.io`，不通再依次试 10 个加速站，**每个 `checkHost` 都有 5 秒超时**。

NAS 上官网常常不通，那就是每次白等 5 秒，量级是这样堆起来的：

| 场景 | 单次探测 | 47 个镜像一轮 |
|---|---|---|
| 官网通（正常） | < 1 秒 | 十几秒 |
| 官网不通、加速站通（常见） | 5.3 秒 | **约 8 分钟** |
| 全部候选不通 | 55 秒 | 最坏约 86 分钟 |

中间的 8 分钟就是「前端 3 分钟超时」的直接来源。

**已优化**：`GetRegistryAddress` 的结论现在按**域名**做了 TTL 缓存（`internal/module/auth.go`）。
依据是这个决策只跟 registry 域名有关、跟具体镜像无关 —— 第一次探到之后，其余 93 次全部命中缓存，
检测从分钟级回到秒级。

| 参数 | 值 | 说明 |
|---|---|---|
| `RegistryHostCacheTTL` | 10 分钟 | 探测成功后的缓存时长 |
| `RegistryHostCacheFailTTL` | 1 分钟 | 候选全不通时的负缓存；取短值，以便加速站恢复后尽快被重新探到 |

显式带了域名的镜像（`ghcr.io/xxx`、私有 registry）直接返回那个域名，**不探测、不缓存、无网络请求**。

### 检测的并发保护

`ImageUpdateData` 内部有一道「同时只跑一轮」的闸（`internal/module/checkupdate.go`）：

| 情况 | 行为 |
|---|---|
| 空闲 | 正常跑一轮 |
| 已有检测在跑 | **等那一轮结束，直接复用它的结论**，不再自己跑一遍 |
| 等到 `CheckRunWaitTimeout`（3 分钟）仍未结束 | 返回 `false`，调用方按需降级 |

三条触发路径（`:30` 定时任务、手动「检测更新」、自动更新前的刷新）都走这道闸，
所以不会叠加出并发的重复检测，也不会出现两份结论互相覆盖。

各调用方拿到 `false` 时的处理：

- **定时任务**：放弃整轮（检测 + 更新都不做），下一个整点再来 —— 这一趟的意义就是「拿刚检测出的结论去更新」，没有新鲜结论就不该动容器；
- **手动接口**：返回 `data.skipped = true`，前端转去轮询接口 6；
- **自动更新前的刷新**：记一行错误日志，沿用现有缓存继续，不因为刷新失败中断本轮更新。

前置动作「自动更新」本身另有一道独立的原子闸（`AutoUpdateRunner.running` 的 `CompareAndSwap`）：
上一轮更新没结束时，下一个 `:30` 的更新阶段直接跳过并记日志，不算失败。

### 防撞车

- 单次执行期间带运行标志，下一轮到点直接跳过，不会任务堆积
- **检测同一时刻只跑一轮**：撞上已有检测时等它结束并复用结论（见上）
- 与**手动更新共用同一把全局锁**，任意时刻只有一台容器在被 stop / rename / create
- 逐台**串行**更新，不并发，避免 Docker daemon 被拉满

### 单台容器的更新流程

1. 更新前 `ContainerInspect` 记下旧容器的 `ImageID` 和原始运行状态（`running` / `exited`）
2. 拉取新镜像
3. 停止旧容器 → 改名 `名字-日期`
4. 用原配置创建新容器并启动
5. **还原原运行状态** —— 原本 `exited` 的容器更新完仍然是 `exited`，不会被擅自拉起
6. 按需删除旧镜像（见下）
7. 删除改名后的旧容器

### 删旧镜像的判定

三个条件同时满足才删：

1. `deleteOldImage` 开关为真
2. 新镜像 ID 与旧镜像 ID **不同**（同一个镜像 tag 变了但 ID 没变时不动）
3. 该旧镜像**已无任何其他容器引用**（遍历容器列表确认）

任一不满足则保留，并在结果里说明原因（如「旧镜像被其他容器引用，已保留」）。

### 失败回滚

`ContainerCreate` 或 `ContainerStart` 失败时：

- 删掉半成品新容器
- 把旧容器改回原名并恢复原运行状态

这是无人值守场景的硬要求 —— 否则一次创建失败就是**静默的服务下线**，用户第二天才会发现容器没了。

### 自身保护

`dockerCopilot` 容器**永不参与自动更新**，否则会在执行过程中把自己重启掉。识别方式是容器名或镜像名包含 `dockercopilot`（大小写不敏感）。此判定优先级高于用户配置的排除列表，即使用户没把它加进排除列表也不会被更新。

---

## 四、构建与部署

前后端已合并到同一个仓库：

```
dockerCopilot/
├── dockercopilot.go
├── internal/          # Go 后端
├── frontend/          # React 前端源码
│   ├── src/
│   ├── vite.config.js
│   └── package.json
└── front/             # 前端构建产物，被 //go:embed front/* 打进二进制
```

`frontend/vite.config.js` 里已把 `build.outDir` 指向 `../front`，所以构建流程只有两步：

```bash
cd frontend
npm install
npm run build      # 产物直接写入 ../front/

cd ..
go build           # embed 进二进制，无需手工拷贝
```

`front/` 已被 `.gitignore` 忽略（构建产物不进版本库），`frontend/node_modules/`、`frontend/dist/` 同样忽略。

### 前端积木位置

| 文件 | 内容 |
|---|---|
| `frontend/src/api/client.js` | `autoUpdateAPI`：`getSetting` / `updateSetting` / `run` / `getCandidates`；`checkUpdateAPI.run`：手动检测（超时放宽到 3 分钟）；`checkUpdateAPI.status`：检测状态轮询 |
| `frontend/src/components/AutoUpdate.jsx` | 自动更新设置面板：开关、排除列表勾选、立即执行、上次执行结果 |
| `frontend/src/components/Containers.jsx` | 容器页顶部「检测更新」与「自动更新」入口按钮 |

### 入口形态

自动更新的入口是容器页顶部工具栏的按钮，点开是**居中浮层面板**，面板内分三个区块：开关组 / 排除列表 / 执行记录。没有做成左侧导航的独立菜单页 —— 勾选排除项时需要对照容器列表，浮层比来回切页更顺手。

---

## 五、本次顺带修复的既有问题

| 位置 | 问题 |
|---|---|
| `internal/module/checkupdate.go` | 遍历 `image.RepoDigests` 时 `needUpdate` 被反复覆盖，最后一次迭代会把 `true` 重置回 `false`，导致多 repo tag 的镜像**漏报更新** |
| `internal/module/checkupdate.go` | 检测结果缓存是裸 map，并发读写存在数据竞争；已加读写锁与检测时间戳 |
| `internal/utiles/image.go` | `log.Fatalf` 会在一次网络抖动时直接打挂整个进程；改为返回 error |
| `internal/module` 进度表 | 任务进度条目只增不删，已加清理，避免长期运行内存无界增长 |
| `frontend/src/components/Containers.jsx` | `ContainerDetailModal` 引用了作用域中不存在的 `setConfirmModal`，触发即崩溃 |
| `frontend/src/components/Containers.jsx` | 「自动更新」面板组件、状态、角标都写好了，但**触发按钮漏写** —— `setShowAutoUpdate(true)` 全项目无一处调用，面板永远打不开 |
| `frontend/public/sw.js` | Service Worker 用 cache-first 且预缓存了 `index.html`，缓存名固定 `docker-copilot-v1` 不升版 → **部署新版本后浏览器一直加载旧页面**。改为 network-first + 缓存版本号 v2，并在 `main.jsx` 监听 `controllerchange` 自动刷新一次 |
| `frontend/public/sw.js` | 接口请求（`/api/`）也被 Service Worker 缓存并参与离线回落：前者让轮询可能读到陈旧响应，后者更糟 —— 离线时 `caches.match('/index.html')` 会把一份 HTML 交给期望 JSON 的调用方。现在 `/api/` 直接放行、完全不进 SW，缓存版本号升到 v3 顺手清掉历史误缓存 |
| `internal/module/checkupdate.go` | 检测没有并发闸：定时任务、手动按钮、自动更新前的刷新可能同时跑同一批镜像。现在同一时刻只跑一轮，撞上时等前一轮结束并复用其结论 |
| `dockercopilot.go` | 定时任务里的 `panic` 会打挂整个进程；改为记录日志后继续 |
