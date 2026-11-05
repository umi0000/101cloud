# 101cloud 宿主机 Agent

Go 节点服务。业务后端只连接 Agent，Agent 通过本机 Unix socket 控制 Incus。用户、规格、pt 账本和跨节点调度留在业务后端。

本目录是独立 Go 模块。以下命令均在 `host-agent/` 目录执行。

初始化、后端接入和各类 API 的完整示例见 [Agent 使用指南](docs/AGENT使用指南.md)。

## 快速开始

```bash
go build -o bin/hostagent ./cmd/hostagent

# endpoint 必须是后端实际访问的地址，带显式端口。
# 目录必须不存在；初始化不连接 Incus、不安装驱动、不修改宿主机网络。
bin/hostagent init --state-dir ./agent-state --endpoint https://127.0.0.1:8443

bin/hostagent -config ./agent-state/agent.json -check
bin/hostagent -config ./agent-state/agent.json
```

初始化自动生成：

| 文件 | 用途 |
| --- | --- |
| `agent.json` | 节点服务配置 |
| `server.crt` / `server.key` | 自签 HTTPS 证书与私钥 |
| `token` | 本节点随机认证密钥 |
| `connection.json` | 供业务后端导入的连接配置，包含地址、Token 和证书指纹 |

**把 `connection.json` 通过可信渠道导入后端即可，不需要客户端证书或 CA。** 它是管理员凭据，不应发给普通用户、提交源码、放进 URL 或记录日志。目录权限为 0700，敏感文件为 0600。初始化只打印文件位置，不打印 Token；重复初始化不会覆盖已有目录。

生产示例（仅供节点管理员执行，本项目开发不会自动执行）：

```bash
hostagent init --state-dir /etc/101cloud-agent --endpoint https://192.168.1.99:8443 --listen 192.168.1.99:8443
hostagent -config /etc/101cloud-agent/agent.json -check
```

默认监听地址由 endpoint 推导。endpoint 是节点可达地址，不能填写 `0.0.0.0`、`::` 等监听通配地址；需要通配监听时用独立的 `--listen`。证书 SAN 根据 endpoint 的 IP 或 DNS 自动生成。默认有效期为一年，可用 `--validity 8760h` 指定；不会在运行中静默更换证书。Incus socket 默认 `/var/lib/incus/unix.socket`，可用 `--incus-socket` 指定。

## 后端导入

连接文件格式：

```json
{
  "version": 1,
  "endpoint": "https://192.168.1.99:8443",
  "token": "<自动生成的节点密钥>",
  "server_cert_sha256": "<自签叶证书DER的SHA256，64位小写十六进制>"
}
```

以上展示格式，示例占位值不是有效凭据。后端 Go 接入：

```go
connection, err := client.LoadConnection("connection.json")
if err != nil {
    return err
}
httpClient, err := client.NewHTTPClient(connection)
if err != nil {
    return err
}
defer httpClient.CloseIdleConnections()
request, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.Endpoint+"/agent/v1/health", nil)
if err != nil {
    return err
}
response, err := httpClient.Do(request)
if err != nil {
    return err
}
defer response.Body.Close()
```

导入 `101cloud/hostagent/client`，请求由客户端自动添加 `Authorization: Bearer ...`。客户端只允许访问该配置的 HTTPS origin，不跟随重定向、不使用环境 HTTP 代理，也没有会中断事件/备份流的全局短超时；调用方通过 context 设置操作期限。

连接配置还可由后端自己的节点录入接口解析为 `client.ConnectionConfig`，交给 `NewHTTPClient` 校验，不要求后端一定使用文件存储。当前项目提供的是 Agent 与 Go 客户端，不包含业务管理网页。

## 安全通信

- TLS 1.3 加密；后端逐次核对叶证书 SHA256 指纹、有效期、DNS/IP 和服务端用途，不接受指纹变化，不自动信任首次连接。
- 自签证书没有公共 CA 信任链，客户端使用显式导入的证书指纹作为信任锚，不是关闭验证后接受任意证书。
- 每个节点使用独立的 256 bit 随机 Token，以 base64url 无 padding 编码。Agent 恒定时间比较认证摘要；所有原生 API、扩展 API和升级通道都必须认证。
- Token 只通过 Authorization 请求头发送，不接受查询参数或 Cookie 认证，认证后不转发 Token 给 Incus。
- 审计不记录请求体、Authorization 和 URL 查询参数；Incus WebSocket 查询中的 secret 同样不会写入日志。
- 完整 Incus 管理权等同于宿主机管理员权限。只向业务后端开放管理端口，仍建议使用管理网/VPN 和来源防火墙。

证书或 Token 泄露时，先停止 Agent，再使用新状态目录重新初始化、更新服务配置并重新导入后端连接信息。不要把重新初始化当作自动信任换证。配置在启动时读取，重启服务会关闭旧连接与 WebSocket，但不停止用户实例。证书到期前须安排显式换证和重新导入；不提供隐式自动换证或旧凭据兼容窗口。

旧的 mTLS CA、客户端证书白名单、`agent-pki` 命令及旧客户端构造接口已移除，旧配置不会自动降级或继续兼容。

## 管理接口

| 路径 | 能力 |
| --- | --- |
| `/`、`/1.0`、`/1.0/*` | 完整 Incus 原生 API，含实例、项目、网络、存储、镜像、备份、操作、事件、exec/控制台 WebSocket 和 SFTP Upgrade |
| `GET /agent/v1/health` | 实时检查 Incus 连通性，不允许匿名访问 |
| `GET /agent/v1/resources` | 完整硬件、GPU负载，以及各接口IP/网段/默认路由/DNS的 `network_interfaces`、`system_dns`、`network_errors` |
| `PUT /agent/v1/instances/{name}/gpu?project=default` | 使用 UUID 配置 GPU，PCI 解析留在本机 |
| `POST /agent/v1/instances/cloud-init/prepare` | 仅为新建、尚未首次启动的停止容器核实 cloud-init 并移除固定禁用标记；虚拟机不适用 |
| `POST /agent/v1/network/ip-probe` | 在指定网卡上探测单个直连 IPv4 的 ARP 冲突，不分配、不预留地址 |
| `GET /agent/v1/instances/metrics?project=default` | 连续采样的实例CPU/内存与隐私安全GPU归属聚合，含新鲜度和质量 |
| `GET /agent/v1/instances/metrics/samples` | 有界内存快照增量补采，按Agent启动ID/序号去重，显式报告重启和缺口 |
| `GET /agent/v1/image-sources` | 固定可信 SimpleStreams 源列表，不接受用户 URL |
| `GET /agent/v1/image-sources/{source}/images` | 从实际目录发现 OS、发行版、架构、容器/虚拟机及 cloud/default 变体 |
| `GET /agent/v1/image-sources/{source}/images/{fingerprint}` | 按完整指纹从同一权威目录解析具体镜像 |

GPU 指标由宿主机 `nvidia-smi` 采集。每卡综合负载为 GPU 利用率与显存占用率的等权平均，节点负载为所有卡分数的等权平均；采样失败用 null 和错误说明表示，不作为空闲。它不是历史时间平均，也不保证仍有足够显存。字段和计算示例见使用指南第 5.2 节。

网络资源按接口索引和名称关联，不把一张卡的网关或DNS混给其他网卡。默认路由保留IPv4/IPv6、表、metric和ECMP权重；DNS区分resolved per-link、全局配置与本机stub，无法确定时报告错误。Linux路由需iproute2，resolved通过本机D-Bus查询，缺失时保留系统级文件后备但不猜per-link归属。后端可据此选择网卡和网络模板，但不能复制宿主IP或从网段推断可分配地址池。详见使用指南第5.2节。

IP 探测请求为 `{"interface":"ens18","address":"192.168.0.112"}`。结果区分发现占用、未发现冲突和未知；`available: true` 仅表示本次未发现冲突，不保证离线设备、DHCP或其他后端没有保留该地址。后端必须维护地址池和并发预留。仅 Linux 支持真实 ARP 探测，需 `CAP_NET_RAW`；更新部署时也要更新 systemd 单元，允许 `AF_PACKET` 和网卡发现所需的 `AF_NETLINK`。不能只替换二进制而沿用旧的地址族限制。详见使用指南第 5.4 节。

原生 API 的版本与扩展以节点 Incus 为准。请求超时不等于操作失败，后端应保存 operation ID 并核对状态，不能盲目重复创建或释放资源。跨节点迁移仍需后端编排连接地址、认证和失败恢复。

远程镜像发现只使用固定 `images`（`https://images.linuxcontainers.org`）源，通过锁定的 Incus v6.0.5 客户端读取实际 SimpleStreams，包括该源中的 Ubuntu 等实际 OS。目录可按 `os`、`release`、`architecture`、`instance_type`、`cloud_init` 过滤；架构使用 Incus 原生名称（如 `x86_64`、`aarch64`）。每条记录保留实际完整指纹与变体，`size_bytes` 是十进制字符串，只有实际 `cloud` 变体标记 cloud-init，默认变体不冒充 cloud。Canonical Ubuntu Cloud 的旧 LXD 元数据不受 SDK 支持，不列为可选源，不重写格式或指纹。不支持的目录格式返回422、源故障502、超时504，不返回伪造发行版或后备镜像。目录最多缓存2分钟，刷新限30秒/64MiB/32次HTTP请求，不跟随重定向、不使用环境HTTP代理。详见使用指南第5.8节。

下载仍走 `POST /1.0/images?project=...` 原生异步API，使用固定源 URL、`protocol: simplestreams` 与所选完整指纹；后端保存 operation ID 并在完成后用同项目 `GET /1.0/images/{fingerprint}` 核对架构/类型。发现目录不代表节点已有镜像，批量节点各自下载/核验，不会创建或改变实例。Agent 不自动安装 GPU 环境，也不改变已有镜像属性。

### 容器负载与隐私

Agent默认每5秒只读采样，采集期限4秒；可通过可选 `metrics` 配置的 `interval_seconds`（1–60）和 `timeout_seconds`（1–30）调整。原配置省略此项仍可启动。仅保留最近120个不可变内存快照，不写临时JSON或本地历史库。慢客户端不会持有采样锁阻塞采集。

CPU用累计计数差分和独立观测时间窗，首次/重启/缺测不构造差分；跨容量变化的窗口保留使用核数但归一化百分比为null，不拿GPU耗时当CPU窗口。GPU在Linux amd64/arm64上无CGO动态读取NVML，内部关联宿主PID/cgroup与设备引用，公开只返回每实例/每卡的统计值及匿名数量，不返回PID、命令行、进程名或路径。配置与实际观察取并集，可看到换卡后留在旧卡上的占用。进程复用、短任务已退出但仍有未归属活动、MPS/MIG/权限或驱动不支持均保守降级，不把无样本当0。

指标快照新增顶层 `host_quality`（`complete`、`attribution_complete`、`reasons`），保留实例行合并前的全节点扫描、上下文归属、设备引用和库存质量；原 `quality` 仍汇总全部实例，包括缺少PID的Stopped实例。因此无关Stopped实例可使 `quality.attribution_complete=false`，但不会单独使 `host_quality.attribution_complete=false`。权限/扫描限额、未归属上下文或近期活动、未知设备引用、MIG归属不全、库存缺失、取消或陈旧采样仍会阻止宿主归属完整；仅缺失可选利用率可使 `complete=false` 而不否定 `attribution_complete`。宿主归属完整不表示GPU空闲。

顶层 `collection_started_at` 为实例/设备输入采集开始前记录的时间，UTC、微秒精度，覆盖后续GPU采样；它不是CPU窗口起点，也不能用 `collected_at` 减耗时替代。当前快照和历史samples都保留该字段，项目过滤不改变这两个宿主字段。原生删除已确认完成后的资源回收须使用 `collection_started_at` 严格晚于完成时间的新鲜、`collection_complete=true`、完整实例列表快照，并要求 `host_quality.attribution_complete` 显式为true、按目标身份核对实例确实缺席；字段缺失/未知不能授权释放，不能仅凭汇总质量或不存在某行释放。

`in_use / idle_candidate / unknown / not_attached` 是观察状态，不会自动释放或换卡。后端策略需用户开启、连续有效窗口及操作前后核对；`idle_candidate`不是原子安全释放授权，CUDA上下文不会迁移。SM利用率是进程样本聚合的近似值，不是算力份额或计费依据。Stopped实例可能因缺少原进程身份而unknown，不强行填0。必须在宿主PID/cgroup/device视图中运行；非root仅有Incus权限不保证有完整进程观测权限。

历史存储新增5张MySQL表（实例/GPU明细、实例/GPU汇总、采集游标），支持可配置bucket_seconds、独立有效时长、加权和/观测峰值及缺测。新库用 [init.sql](../backend/sql/init.sql)，已有库用 [指标升级脚本](../backend/sql/migrations/20261029_instance_metrics.sql)。本次没有实现尚未开发的业务后端入库器、历史查询服务或前端图表；DDL不会自动收集、聚合或清理历史。详细参数、隐私边界、计费与指标区别见使用指南第5.5–5.7节。

### GPU UUID配置

```json
{
  "gpu_uuids": ["GPU-7f665bf9-5911-4f71-ac6a-a66e32362574"],
  "allow_live": false
}
```

- 多 UUID 为多卡，空数组移除 Agent 管理的显卡配置；只管理 `agent-gpu-N`，不静默删除其他 GPU 或 profile 配置。
- 默认要求实例已停止。`allow_live: true` 显式允许在线改入口，**不迁移 CUDA context、不杀旧进程、不保证旧卡显存释放**。
- CPU 工作空间以后要在线接卡，应在首次启动前准备 `nvidia.runtime=true` 和库环境；热插卡不重新执行 NVIDIA 启动 hook。
- 使用 ETag 防止覆盖配置；返回 `operation_id`、`operation_completed`、`configuration_state`、`actual`、`etag`，失败不假定已回滚。
- `resource_release` 始终是 `not_verified`，配置成功不等于设备挂载和进程状态一致。配额、共享人数、计费和实际占用核验仍由业务层组织。

### 新实例首次启动前的 cloud-init 准备

受信任后端仅在创建流程、身份与配置核实完成后、首次启动前调用：

```http
POST /agent/v1/instances/cloud-init/prepare
Content-Type: application/json

{"project":"tenant","name":"new-instance","expected_uuid":"INCUS-UUID","expected_instance_id":"42","expected_image_fingerprint":"完整的64位镜像指纹"}
```

成功返回 `{"checked":true,"disabled_marker_removed":true}`；标记已经不存在（包括并发删除返回类型化 404）时返回 `disabled_marker_removed:false`。五个字段均必填且为非空字符串；拒绝未知字段、重复字段、尾随 JSON 和查询参数，项目必须显式提供，不默认到 `default`。

Agent 在指定项目内读取实例，并严格要求名称、项目、`volatile.uuid`、本地 `user.101cloud.instance_id` 归属标记及完整 `volatile.base_image` 均匹配，且状态文字和状态码均为 Stopped。只支持 `container`；停止虚拟机文件访问不受支持，返回 422，后端必须显式跳过虚拟机，不能把此能力当作所有实例的必需启动前置条件。身份/状态冲突返回 409，未安装 cloud-init 返回 422，其余文件访问/权限/传输错误失败关闭。

通过 Incus 文件 API 读取固定 `/usr/bin/cloud-init`、`/etc/cloud/cloud.cfg` 两个安装凭据（均须为文件），以及固定 `/etc/cloud/cloud-init.disabled`。在移除禁用标记前，检查 `/etc`、`/etc/cloud`、`/etc/cloud/cloud.cfg.d` 均为 root:root 所有且非组/其他可写的真实目录，然后仅在固定 `/etc/cloud/cloud.cfg.d/99-101cloud-preserve-hostname.cfg` 缺失时写入固定 `preserve_hostname: true\n`（root:root、0644）。已有文件仅在类型/所有权/模式和内容全部精确匹配时幂等跳过；冲突返回409，不覆盖客机配置。该系统级配置保证 cloud-init local 阶段早于 user-data 合并时也保留 Incus 分配的 hostname；仅 user-data 设置不足以阻止该早期动作。写入前及删除标记前再次核实身份和停止状态，写后只读确认。只有 Incus SDK 的类型化 HTTP404才表示缺失；403、超时或无效响应均失败关闭。同项目同名称操作使用现有实例锁串行化。不执行 guest exec、SSH、包安装、SSH keygen，也不改密码/密钥或管理脚本。

此端点不是通用修复接口：调用者负责将它限定在新建任务首次启动前，不得对源镜像、既有实例、重启或登录配置更新调用。它不会修改镜像、启动实例或执行首次启动逻辑；Incus 原生 API 的外部并发写入不受 Agent 实例锁保护。

## 构建与部署

需要 Go 1.24 或更新版本，Incus SDK 锁定 v6.0.5。

```bash
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/hostagent-linux-amd64 ./cmd/hostagent
```

`deploy/agent.example.json` 展示生成配置的结构，相对证书/Token 路径相对于配置文件目录。生产推荐直接使用初始化生成的配置，避免手写密钥和证书路径。

`deploy/101cloud-agent.service` 使用 `/usr/local/bin/hostagent` 和 `/etc/101cloud-agent/agent.json`。默认 root 运行并启用系统限制；专用用户加 Incus socket 管理组也属于高权限，不能视为宿主机隔离边界。

本地测试与协议烟测不代替真实节点验收。未经明确授权，不 SSH，不在服务器安装或启动 Agent；真实 Incus 实例、备份恢复和 NVIDIA 计算需在获得授权后验收。

## Ubuntu 宿主机 AppArmor 兼容性与持久化

### 适用情况

Ubuntu 25.04/25.10/26.04 宿主机上，Incus 非特权容器可能出现真实 SSH/PTY 内 `apt update` 挂起，但无终端的 Agent exec 可以成功的情况。已实测 Ubuntu 26.04 宿主机、Incus 6.0.5、Ubuntu 24.04 容器：APT 的 Ubuntu Pro 前置钩子启动 `systemctl`，其终止 `systemd-tty-ask-password-agent` 的信号被 AppArmor 拒绝，导致更新在下载前等待。

不能只凭 apt 慢就套用此方案。应先检查进程链与宿主机内核日志，确认 `apparmor="DENIED"`、`operation="signal"`、`comm="systemctl"` 和相关容器标签；网络、软件源、锁等待等其他问题需分别排查。Agent 不会自动修改下述参数。

Ubuntu 24.04 **宿主机**通常默认不启用这一项额外限制，因此可作为候选基线，但不保证所有更新、加固配置或内核/Incus组合均无此问题。仅把容器系统换为 Ubuntu 24.04 不足以规避宿主机限制。我们尚未在 Ubuntu 24.04 宿主机上完成同等验收。

### 检查与持久化

以下命令在 **Ubuntu 宿主机**执行，不是在容器中执行；先记录原值：

```bash
sysctl kernel.apparmor_restrict_unprivileged_unconfined
sysctl kernel.apparmor_restrict_unprivileged_userns
```

确认接受下面的安全取舍后，创建独立配置并只加载此文件：

```bash
sudo tee /etc/sysctl.d/99-incus-apparmor-compat.conf >/dev/null <<'EOF'
# Compatibility for Incus with Ubuntu's extra AppArmor restrictions.
kernel.apparmor_restrict_unprivileged_unconfined = 0
EOF

sudo sysctl -p /etc/sysctl.d/99-incus-apparmor-compat.conf
sysctl kernel.apparmor_restrict_unprivileged_unconfined
```

预期输出 `kernel.apparmor_restrict_unprivileged_unconfined = 0`。系统启动时会加载该配置；如其他 sysctl 配置也设置了同一项，需要检查加载顺序，避免后加载的文件覆盖回 1。不要顺带将 `kernel.apparmor_restrict_unprivileged_userns` 设为 0；本次验证中它始终保持 1。

### 现有容器与验收

仅修改运行时参数、重新 SSH，不一定刷新既有容器的安全上下文。本次在参数为 0 时，**通过 Host Agent 仅重启受影响容器**后，真实 SSH 下普通 `apt update` 才恢复，2.88 秒退出 0；systemd 与 CUDA 复验通过。

重启使用 Agent 代理的原生接口 `PUT /1.0/instances/{name}/state?project=default`，请求体为 `{"action":"restart","timeout":30,"force":false,"stateful":false}`。应等待异步操作成功及 systemd 就绪，再用真实 SSH/PTY 验证；不能仅用非交互 exec 判定修复。重启会中断该容器的会话和任务，须先确认可停机，不要批量重启其他实例。仅重启 Agent 服务不会替代容器重启。

如果运行时参数已经是 0、受影响容器也已重启验证，写入持久配置本身不需要再次重启容器或宿主机。

### 安全影响与回退

设为 0 **不是关闭整个 AppArmor**，也不取消非特权容器、UID/GID映射、seccomp、capabilities或资源配额。但它会在整台宿主机范围取消一层针对低权限 unconfined 进程切换 AppArmor profile 的额外防绕过限制，不只影响某一个容器。仍保持 userns 开关为 1，不等于这层防绕过能力没有减弱。多人可执行任意代码的平台应明确评估该取舍，不能把它描述成无安全影响的配置。

长期应验证兼容的内核、AppArmor和Incus版本组合，不能未经测试承诺升级一定修复。原理和上游讨论见 [Ubuntu 安全团队说明](https://discourse.ubuntu.com/t/understanding-apparmor-user-namespace-restriction/58007)、[Ubuntu 缺陷报告](https://bugs.launchpad.net/bugs/2109394)。

若该文件确实由上述步骤新建、且没有加入其他自定义项，可以这样撤销；运行时值应恢复为你事先记录的原值，下面以原值 1 为例：

```bash
sudo rm /etc/sysctl.d/99-incus-apparmor-compat.conf
sudo sysctl -w kernel.apparmor_restrict_unprivileged_unconfined=1
```

回退可能重新触发兼容性问题，也不能把已有进程的上下文恢复视为即时完成；需安排受影响容器的重启与复验。不要删除其他系统或管理员提供的 sysctl 文件。本节只是人工操作指南，Agent 初始化和启动不会自动执行这些变更。

## 本地验证记录

早期协议验收通过全包竞态测试、`go vet`、构建与Linux amd64交叉构建，执行了初始化、重复初始化拒绝和配置预检；使用真实Agent与Go后端客户端，通过导出的连接文件访问health、原生实例API和WebSocket。错误Token返回401，错误指纹在发送HTTP请求前被拒绝，Token未转发到Unix socket上游。该阶段Incus上游为本地协议测试服务；后续真实节点部署和GPU负载验收见下文。

### 容器指标验收记录

此前的只读验收通过全包竞态/静态检查、无CGO Linux amd64/arm64构建，以及Linux上的monitor、GPU reader、node回归测试；当时没有启动GPU负载。以下为后续真实负载验收结果。

此前MySQL 8.4.11临时库验证了37表初始化、原32表增量升级、幂等键、历史快照外键、CPU窗口和缺测/汇总约束，验证库已删除。

2026-10-29在192.168.0.111升级了正式Agent，保留原证书与Token；复用运行中的MySQL 8.4.11并初始化37表空业务库101cloud。真实CUDA负载暴露并修复systemd的init.scope与system.slice兄弟层级归属问题：使用内核cgroup挂载根确定容器边界，不再把PID 1的叶子组当成整个容器。另将上下文归属与设备引用完整性分开，保留已确认的进程数/显存/SM统计，但通用控制设备引用仍阻止空闲释放。

隔离容器agent-gpu-metrics-accept-fe25c31b实测GPU进程数1、显存220MiB、近似SM利用率95–96%；停止负载后回到idle_candidate、进程数0、显存0，最后已停止测试容器，未启停原有实例。最终代码通过全包竞态测试、go vet、Linux amd64/arm64构建及真实Linux monitor回归；最终部署产物再次验证了繁忙到空闲转换及公开响应隐私。

临时采集任务把真实数据保留在101cloud_metrics_accept_fe25c31b：150条实例样本、150条GPU样本、各1条日报汇总、1条增量游标；包含3个Agent启动流及12条进程数未知的样本。重复写入相同批次不增加样本或改变指标，未知值保持NULL，汇总按有效时长加权，不用未知点补零。正式业务库不写入验收业务数据。本次没有实现或部署长期后端采集器，采样验收结束后不会持续写MySQL；该库用于回看验收结果。升级备份保存在节点/var/lib/101cloud-agent-backups/fe25c31b，临时测试程序已移除。
