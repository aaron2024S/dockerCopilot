# dockerCopilot 离线镜像构建与部署

本机（以及很多 NAS 场景）没有 Docker 引擎，`docker build` 用不了。
`offline/` 下的这套工具**不依赖 Docker**，直接按 Dockerfile 的语义
**手工拼装 `docker save` 格式的镜像归档**，产物在目标机上 `docker load -i` 即可导入。

---

## 一、产物

| 文件 | 说明 |
|---|---|
| `dockercopilot-v2.1.3-amd64-image.tar` | x86_64 镜像（docker-save 格式），`docker load` 直接导入 |
| `dockercopilot-v2.1.3-arm64-image.tar` | ARM64 镜像，同上 |
| `docker-compose.offline.yml` | 离线部署用 compose（**不含 build 段**），跟 tar 一起拷到目标机 |
| `SHA256SUMS.txt` | 产物 sha256 清单，用于核对传输完整性 |
| `fetch_base.py` | 拉取基础镜像层与 tzdata |
| `build_offline.py` | 组装镜像 tar |
| `validate_image.py` | **静态**校验：结构 / 层 sha256 / 关键文件 / 启动链路 |
| `validate_boot.py` | **行为**校验：把启动脚本真跑一遍 |
| `build.ps1` | Windows 一键跑完整条链路 |

导入后镜像**只带一个标签**：`dockercopilot:latest`。
（不再附加 `dockercopilot:vX.Y.Z` —— 反复 load 不同版本会在宿主机上堆出一串 tag，
而 compose 引用的始终是 `latest`。版本信息仍保留在 tar 文件名、镜像 config 的 LABEL
以及镜像内 `/app/version` 里，想知道当前跑的是哪版可以看界面上显示的版本号。）

> 想换成上游的名字好让原有 compose 直接复用：
> `docker tag dockercopilot:latest 0nlylty/dockercopilot:latest`

---

## 二、目标机部署（三步）

```bash
# 1. 导入镜像（按目标机架构选对应的 tar）
docker load -i dockercopilot-v2.1.3-amd64-image.tar

# 2. （可选）核对完整性
sha256sum -c SHA256SUMS.txt

# 3. 启动
docker compose -f docker-compose.offline.yml up -d
```

启动前**务必**改掉 `docker-compose.offline.yml` 里的 `secretKey`（登录密码）。
`./data` 会自动创建，里面放备份、镜像 logo 配置和自动更新配置
（`data/config/autoUpdate.json`）。

升级：导入新版 tar 后重新 `up -d`，`./data` 不受影响。

### 一分钟自检

```bash
docker load -i dockercopilot-v2.1.3-amd64-image.tar
docker run --rm -p 12712:12712 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e secretKey=abcd12345 dockercopilot:latest
# 看到 go-zero 启动日志、无报错即为正常，Ctrl+C 退出
```

然后 `curl -I http://127.0.0.1:12712/manager` 期望 200。

---

## 三、校验结果（本机实测）

默认架构（amd64）每次都跑静态 + 行为两轮校验，全部通过：

| 检查 | 结果 |
|---|---|
| 静态校验（52 条断言） | PASS |
| 行为校验（启动链路 16 项） | PASS |
| 镜像大小 | 约 31 MB |
| 层数 | 2（基础 1 + 应用 1） |
| 归档条目总数 | 1145 |
| sha256（最近一次构建） | `36f362ea…b1234a` |

用 `-Arch all` 时两个架构会各自跑一遍，arm64 的校验项目与 amd64 相同。arm64 镜像需显式指定才会构建。

（历史 sha256 是在 `SOURCE_DATE_EPOCH=1789603200` 下构建的，见第六节。）

---

## 四、与上游 `docker/Dockerfile` 的对应关系

镜像不是"重新发明"，而是逐条复刻 Dockerfile 语义：

| Dockerfile 指令 | 离线构建里怎么做的 |
|---|---|
| `FROM alpine` | `fetch_base.py` 走 registry v2 API 拉 `library/alpine` 的层，逐层校验 `sha256(明文) == diff_ids[i]` |
| `COPY dc-back/dist/$TARGETPLATFORM/dockerCopilot` | 从 `dc-back/dist/linux/<arch>/` 取，**校验 ELF 架构**防止张冠李戴，写入 `app/dockerCopilot` (0755) |
| `COPY dc-back/etc/. ./etc` | `etc/` 整目录复制到 `app/etc/` |
| `COPY start.sh ./start.sh` | `app/start.sh` (0755)，内容逐字节保持上游原样 |
| `RUN apk add --no-cache tzdata` | 离线无法执行 apk，改为下载 apk 包、**拆出 data 段直接铺层**（见第五节） |
| `WORKDIR /app` | `config.WorkingDir = "/app"` |
| `ENV secretKey / DOCKER_HOST / BACKUP_DIR / TZ` | 逐条写入 `config.Env` |
| `VOLUME ["/data"]` | `config.Volumes = {"/data": {}}` |
| `CMD ["./start.sh"]` | `config.Cmd = ["./start.sh"]`，无 ENTRYPOINT（与上游一致） |
| `LABEL authors="onlyLTY"` | `config.Labels`（另加 OCI 标准 label：title / version / source / license） |

`ExposedPorts` 是唯一"多出来"的（上游 Dockerfile 没写 `EXPOSE`）：这里按
`etc/dockerCopilot.yaml` 的实际端口补了 `12712/tcp`。`EXPOSE` 只是元数据，
不影响端口映射，但能让部分 NAS 面板自动提示端口。

---

## 五、构建原理

### 1. 基础镜像：走 registry v2 HTTP API，不调 docker 命令

`fetch_base.py`：

1. `GET /v2/library/alpine/manifests/latest` 拿多架构 index
   → 首次请求会 401，按 `WWW-Authenticate` 头换匿名 Bearer token 后重放
   （这是 registry 的标准流程，**不换 token 直连会被 401 挡住**）
2. 从 index 里挑 `linux/<arch>` 子 manifest，校验其 digest
3. 下载 config blob 与各层 blob，**双重校验**：
   `sha256(gzip blob) == manifest.layers[i].digest`，
   且 `sha256(解压后的明文 tar) == config.rootfs.diff_ids[i]`
4. 从层里读 `/etc/alpine-release` 得到 Alpine 版本，用于下一步选软件源分支

镜像站按顺序尝试 `docker.m.daocloud.io` / `hub.rat.dev` / `docker.1ms.run` /
`dockerproxy.net`，第一个成功的就用。产物缓存进 `offline/cache/<arch>/`。

> `dockerproxy.net` 排在最后：它不要求鉴权，但按 digest 取子 manifest 会返回 500。

### 2. tzdata：拆 apk 而不是装包

上游有 `apk add --no-cache tzdata`（`TZ=Asia/Shanghai` 要能生效）。
离线组装无法执行 apk，改成：

1. 从 Alpine 软件源（清华 / 阿里 / 官方 CDN）拉 `APKINDEX.tar.gz`，
   解析出 tzdata 的版本号与大小
2. 下载对应的 `.apk`，校验大小与 `C:` 字段的校验和
3. 拆出 apk 里的文件树，直接铺进应用层

**这一步有两个容易踩的坑：**

- **apk 是三段拼起来的**：可选签名段 → control 段（含 `.PKGINFO`）→ data 段。
  三段都是合法 gzip 流且是**连续拼接**的，标准 gzip 库只读第一个成员。
  必须用 `zlib.decompressobj` 逐个拆（靠 `unused_data` 反推下一段起点）。
  另外签名段本身也是合法 tar，**不能只凭"能解析成 tar"判断哪段是文件树**
  —— 否则会把 2 KB 的签名当成 1.5 MB 的 zoneinfo 塞进去。
- **`C:` 校验和的算法**：实测确认它是 **control 段"原始压缩字节"的 SHA1**
  （不是解压后的明文，也不是整个 apk 文件）。搞错会一直误报校验失败。

### 3. 组装应用层

`build_offline.py` 把 `app/dockerCopilot`、`app/etc/`、`app/start.sh` 和 tzdata
的文件树打成一个 tar，算 `diff_id = sha256(明文 tar)`，追加到
`config.rootfs.diff_ids`，再按 docker-save 规范拼出
`<层id>/{VERSION,json,layer.tar}` × N + `<config-sha>.json` + `manifest.json` + `repositories`。

tzdata 的条目是**从 tar 直接搬运**而不是"解包到磁盘再打包"：
apk 的 data 段里有符号链接（时区别名），Windows 上没有创建 symlink 的权限，
走文件系统会丢链接。

### 4. 两道校验（这一步不能省）

- `validate_image.py` 只读字节：层 sha256、关键文件在不在、
  `dockerCopilot` 是不是目标架构的 ELF 且带可执行位、
  `start.sh` 是不是纯 LF、config 字段对不对、
  CMD 的相对路径在 WorkingDir 下能不能解析、
  层里有没有 Windows 路径残留
- `validate_boot.py` 真跑：用一个桩程序替换真实二进制，
  在临时目录里复现镜像内的目录结构和环境变量，执行 `sh start.sh`，验证
  工作目录真的切过去了、`./dockerCopilot` 相对路径解析正确、
  自更新分支（`dockerCopilot-new` 覆盖）能工作、换调用者 cwd 行为不变

分两道是有教训的：**静态检查全绿、容器一启动就崩**是这类手工组装最容易出的问题
（cwd 残留 → 入口相对路径解析失败 → 无限重启）。所以启动链路必须真跑一遍。

---

## 六、Windows 无 Docker 环境下的重建

### 一键（推荐）

```powershell
cd D:\workbuddy\dockerCopilot\offline

# 用现有二进制，重建 amd64 镜像并校验（默认只做 amd64）
.\build.ps1

# 从源码全量重建（含前端 + Go 二进制）
.\build.ps1 -CrossCompile -Frontend

# 需要其它架构时显式指定
.\build.ps1 -Arch arm64
.\build.ps1 -Arch all
```

### 手工三步

```powershell
# 1) 前端产物（本机 vite 的默认压缩阶段会确定性死锁，必须关掉 minify）
cd frontend ; npm run build -- --minify false
#    产物直接写入 ../front/，也就是 //go:embed 的目标目录

# 2) 交叉编译（默认只做 amd64；需要 arm64 就改 GOARCH 再来一遍）
$env:CGO_ENABLED=0; $env:GOOS=linux
$env:GOARCH='amd64' ; go build --trimpath -ldflags='-w -s ...' -o dc-back/dist/linux/amd64/dockerCopilot .

# 3) 拉基础层 → 组镜像 → 双校验
cd offline
python fetch_base.py    --arch amd64
python build_offline.py --arch amd64
python validate_image.py
python validate_boot.py
```

### 可复现构建

归档里的 mtime 与 config 的 `created` 默认取当前时间，所以每次构建的哈希都不同。
设 `SOURCE_DATE_EPOCH`（Unix 秒）可固定它们，**同样输入产出逐字节相同的 tar**：

```powershell
$env:SOURCE_DATE_EPOCH = 1789603200   # 2026-09-17 UTC
.\build.ps1
# 或： .\build.ps1 -SourceDateEpoch 1789603200
```

已验证：连跑两轮，两个架构的 sha256 完全一致。要让哈希成为可信基准，
发布时就用固定 epoch 构建。

**离线重建的前置条件**：一旦 `offline/cache/<arch>/` 里已有所需内容
（alpine 层 + tzdata），后续重建**全程不需要联网**。

---

## 七、与上游的唯一有意差别

镜像里额外写入了 `ENV WORKDIR=/app`。

原因：上游 `start.sh` 第一行是

```sh
cd "${WORKDIR}" || exit
```

但 `WORKDIR` 只是 **Dockerfile 指令**（用来设容器工作目录），**并不是环境变量**
——上游 Dockerfile 也没定义它。所以官方镜像里这个变量是空的，脚本执行的是
`cd ""`。空字符串参数下 `cd` 的行为**取决于 shell 实现**：有的当无操作，
有的报错（报错时 `|| exit` 会让容器直接退出）。

补上这个环境变量后，`cd /app` 在任何 shell 下都确定成立。`start.sh` 本身
**保持逐字节原样**，方便和上游 diff。

`validate_boot.py` 的 G 组会打印宿主 sh 对空 `WORKDIR` 的实际行为，
用来提示这个风险的来源。

---

## 八、限制与已知问题

- **架构**：默认只构建 amd64（x86_64），不再顺带产出 arm64，避免多打一份用不上的镜像。
  确实需要 ARM 版时显式加 `-Arch arm64`；不确定目标机架构就 `uname -m` 看
  （`x86_64` → amd64，`aarch64` → arm64）。
- **前端是未压缩产物**：本机 esbuild 压缩阶段会死锁，只能用 `--minify false`，
  代价约 0.6 MB。在能正常跑 esbuild 的机器上重跑默认 `npm run build` 再执行
  第 3 步即可得到压缩版。
- **`app-layer-<arch>.tar` 是中间产物**（各约 22 MB），保留是为了方便排查；
  重新构建会覆盖。已在 `.gitignore` 里忽略。
- **镜像不会自动更新**：本工具自身作为容器运行时，部署用的镜像是这份手工产物；
  要升级需重新构建 tar 并 `docker load`。
- **`privileged: true`**：沿用上游官方 compose。仅管理同宿主 Docker 的话，
  可以删掉它只保留 docker.sock 挂载。

---

## 九、排错

| 症状 | 处理 |
|---|---|
| `docker load` 报校验失败 | 先 `sha256sum -c SHA256SUMS.txt` 确认是传输损坏；损坏就重新拷 |
| 容器起来就退出 | `docker logs dockercopilot` 看日志。若是 `can't cd`/入口找不到，说明工作目录没切过去 —— 确认镜像里 `ENV WORKDIR=/app` 在（`docker inspect` 可查） |
| 时区不对 | 确认 `TZ` 环境变量生效、镜像里 `/usr/share/zoneinfo/Asia/Shanghai` 存在 |
| 拉基础层全部镜像站失败 | 换网络重试，或在能联网的机器上跑一次 `fetch_base.py` 后把 `offline/cache/` 整目录拷过来 |
| `fetch_base.py` 报 manifest digest 不匹配 | 上游 `alpine:latest` 在此期间更新过，删掉 `cache/<arch>/` 重跑即可 |
