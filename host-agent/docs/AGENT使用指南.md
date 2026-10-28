# 宿主机 Agent 使用指南

本文面向节点管理员和业务后端开发者。Agent 使用 **自签 HTTPS + 每节点 Token + 预先导入的证书指纹**；不需要 CA 或客户端证书。

本指南中的“项目根目录”指独立模块目录 `host-agent/`，构建、初始化和请求工具命令均从该目录执行。

## 1. 先分清两种接口

| 类型 | 路径 | 返回格式与职责 |
| --- | --- | --- |
| Agent 扩展接口 | `/agent/v1/*` | Agent 自己的 JSON，提供健康检查、资源发现、GPU UUID 配置 |
| Incus 原生接口 | `/`、`/1.0`、`/1.0/*` | 保持 Incus API、响应封装、异步操作和版本扩展语义 |

不存在独立的 `/agent/v1/instances` CRUD、登录或充值接口。实例、镜像、网络、存储等通过 `/1.0/*` 管理；用户权限、套餐、pt、GPU 共享名额由业务后端负责。

**一份节点连接配置拥有完整节点管理能力。** 普通成员不能直接访问 Agent，后端必须先校验业务权限。Agent 不会替后端限制成员只能操作自己的实例，也不限制原生 API 调用者只能使用 UUID GPU 接口。

## 2. 安装、初始化和启动

### 2.1 前提与构建

- Go 1.24 或更新版本；节点使用 Linux，Agent 可从其他系统交叉构建。
- 节点已安装并配置 Incus；运行账户可访问其 Unix socket。
- GPU 场景另需宿主机驱动、NVIDIA 容器库及兼容容器镜像。Agent 不安装这些组件，也不修改 AppArmor、网口或宿主机路由。

在项目根目录构建：

```bash
go build -o bin/hostagent ./cmd/hostagent
# 若目标是 Linux amd64：
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/hostagent-linux-amd64 ./cmd/hostagent
```

### 2.2 初始化

下面是本地地址示例；生产中必须替换为后端真正访问的节点地址。

```bash
bin/hostagent init \
  --state-dir ./agent-state \
  --endpoint https://127.0.0.1:8443
```

| 参数 | 默认值 / 要求 |
| --- | --- |
| `--state-dir` | 必填，新目录；已存在时拒绝覆盖，即使是空目录 |
| `--endpoint` | 必填，`https://主机:端口`；明确端口，无路径、末尾斜杠、用户名、查询或 fragment |
| `--listen` | 默认 endpoint 的主机和端口；可以独立指定监听地址，如 `0.0.0.0:8443` |
| `--incus-socket` | `/var/lib/incus/unix.socket`，必须是绝对路径 |
| `--validity` | `8760h`，自签证书有效期，使用 Go duration，不支持 `365d` |

endpoint 不能用 `0.0.0.0` 或 `::`；它们只是监听通配地址，不是后端的连接目标。证书 SAN 根据 endpoint 的 DNS/IP 生成，随后不能随意改用另一个名称连接。状态目录的父目录须存在。

生成的目录为 0700，各文件为 0600：

```text
agent-state/
├── agent.json       节点配置
├── server.crt       自签服务端证书
├── server.key       服务端私钥，不能交给后端
├── token            节点随机密钥
└── connection.json  后端导入材料，包含地址、Token、叶证书 SHA256 指纹
```

初始化不监听、不连接 Incus、不输出 Token，只打印文件位置。请通过可信渠道传递 `connection.json`；不能从一个尚未认证的 HTTP 页面获取它再盲目信任。

### 2.3 预检与启动

```bash
bin/hostagent -config ./agent-state/agent.json -check
bin/hostagent -config ./agent-state/agent.json
```

`-check` 仅检查配置、服务端证书、私钥和 Token，不证明 Incus 可达。默认配置路径是 `/etc/101cloud-agent/agent.json`。

生产中可按 [systemd 单元](../deploy/101cloud-agent.service) 安装二进制和配置，然后由管理员执行：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now 101cloud-agent
sudo journalctl -u 101cloud-agent --no-pager -n 50
```

服务单元需要先安装为 `/etc/systemd/system/101cloud-agent.service`，二进制为 `/usr/local/bin/hostagent`，生成配置位于 `/etc/101cloud-agent/agent.json`。不要仅复制示例 JSON 而漏掉证书和 Token。限制管理端口来源；不要向成员暴露 Incus socket。

## 3. 后端导入与请求方式

### 3.1 Go 客户端

`client.LoadConnection(path)` 严格读取 0600 的连接文件；`client.NewHTTPClient(connection)` 校验字段并创建客户端，自动加入 Bearer Token。

客户端只允许同一个 HTTPS origin，不使用环境 HTTP 代理，不跟随重定向；TLS 1.3 握手时校验证书指纹、有效期、主机名和用途，成功后才发送 HTTP 请求和 Token。没有自动 TOFU 或接受换证。

后端若已将导入内容存入数据库，可构造 `client.ConnectionConfig` 交给 `NewHTTPClient`，无需每次读取文件。连接配置版本目前为 1；字段是 `version`、`endpoint`、`token`、`server_cert_sha256`。后者是叶证书 **DER** 的 SHA256 小写 hex，不是 PEM 文本或公钥的摘要。

### 3.2 请求体直接发送

后续示例中的 JSON 是 HTTP 请求体，业务后端直接在内存中编码并发送，无需先保存为文件。使用 Go 客户端时，认证头会自动加入；JSON 请求设置 `Content-Type: application/json`。响应可能包含终端 secret，不要无差别写入日志。


## 4. 通用协议规则

### 4.1 认证与错误

所有请求，包括 health、WebSocket、SFTP Upgrade，都必须经 HTTPS 携带唯一的 `Authorization: Bearer <Token>`。不接受 query/Cookie Token；使用 Go 客户端时无需自己加入认证头。

| 情况 | 行为 |
| --- | --- |
| 指纹、有效期、主机名错误 | 客户端 TLS 阶段拒绝，没有 HTTP 状态码 |
| 缺失、错误或重复 Authorization | HTTP 401，`WWW-Authenticate: Bearer realm="hostagent"` |
| Agent 扩展路径不存在 / 方法错误 | 404 / 405，方法错误带 `Allow` |
| 扩展接口参数、UUID、JSON 错误 | 通常 400；GPU JSON 超过 64 KiB 为 413 |
| GPU 配置冲突、实例未停止、节点不匹配 | 409 |
| 配置已被其他客户端修改 | Incus ETag 冲突可返回 412 |
| Incus 连接失败 | health 为 503；原生代理通常为 502；其他扩展按错误路径返回 502/503 |
| GPU 等待锁时请求期限耗尽 | 408；操作发出后的失败/超时不可据此假定已回滚 |

Agent 扩展错误通常为 `{"error":"错误代码","message":"说明"}`；GPU 操作发出后失败，会使用第 5.3 节的结果对象附带错误。代理自身错误可能是纯文本，不能假定所有错误响应都是 JSON。

原生 Incus 响应有自己的封装：同步成功在 `metadata` 中，异步通常 HTTP 202 并带 `operation`，错误含 `error` / `error_code`。它们不是 Agent 扩展对象。

### 4.2 项目、资源名与 ETag

- 所有有项目语义的原生请求显式加 `?project=...`，不能只在创建时加；默认项目是 `default`。
- GPU 扩展仅接受一个 `project` 查询参数，拒绝未知查询参数；health/resources 是节点级接口，不用 project 选择其他节点。
- 路径资源名应使用 URL path escaping，查询值使用 query escaping；例如镜像别名 `ubuntu/24.04` 作为一个资源名需要转义为 `ubuntu%2F24.04`。
- 更新/删除可使用 GET 返回的 `ETag`，通过 `If-Match` 原样携带引号。412 后重新读取、核对再决定，不盲目覆盖。
- `PUT` 应发送该资源的完整可写对象，不要把 GET 的全部只读字段或仅一项配置直接作为 PUT。使用 PATCH 时核对该资源的合并语义。

### 4.3 异步操作

以响应中的实际 operation ID 为准：

```http
GET /1.0/operations/实际ID?project=default
GET /1.0/operations/实际ID/wait?project=default&timeout=30
```

`wait` 的 HTTP 成功不等于任务成功：检查 `metadata.status_code`、`metadata.err`，成功终态为 `200`，运行中常见 `103`，失败终态常见 `400`。不要把响应封装最外层的 `status_code` 当作任务状态。

客户端超时、断连、Agent 重启都不等于 Incus 已取消任务。后端保存 ID、重新查询、核对资源后处理。`DELETE /1.0/operations/{id}` 只是请求取消，只有可取消的操作才支持。

## 5. Agent 自有接口

### 5.1 `GET /agent/v1/health`

实时读取本地 Incus 服务信息，内部期限 10 秒。成功示例（`server_name` 由节点决定）：

```json
{"status":"ok","incus":true,"server_name":"gpu-node-1"}
```

失败为 503 和 `incus_unavailable`。这不是匿名探针，也不证明 GPU、存储或容器工作正常。节点失联时不要自动认定实例停止、名额释放。

### 5.2 `GET /agent/v1/resources`

```http
GET /agent/v1/resources
```

对象包含：

| 字段 | 内容 |
| --- | --- |
| `server_resources` | Incus 完整硬件资源对象：CPU、内存、GPU、网络、存储等，取决于版本 |
| `gpus` | 所有 Incus 枚举的 GPU，每卡包含型号、UUID/PCI 和可用的运行指标，不限于 NVIDIA 卡 |
| `average_load_percent` | 所有 GPU 综合负载的算术平均，0–100；任一卡负载未知或无卡时为 null |

每卡字段：`model`、`uuid`、`pci_address`、`utilization_percent`、`memory_used_mib`、`memory_total_mib`、`memory_utilization_percent`、`load_percent`，采集失败时另有 `error`。非 NVIDIA 卡可能没有 UUID；UUID 分卡接口仍仅支持 NVIDIA。旧的 `nvidia_gpus` 字段已移除，后端改读 `gpus`。

每卡对象示例（仅示意计算，不是实时测量结果；完整响应另有 `server_resources`）：

```json
{
  "gpus": [
    {
      "uuid": "GPU-7f665bf9-5911-4f71-ac6a-a66e32362574",
      "pci_address": "0000:05:00.0",
      "model": "NVIDIA TITAN X (Pascal)",
      "utilization_percent": 80,
      "memory_used_mib": 4000,
      "memory_total_mib": 10000,
      "memory_utilization_percent": 40,
      "load_percent": 60
    }
  ],
  "average_load_percent": 60
}
```

计算规则固定为：

- 显存占用率 = `100 × memory_used_mib / memory_total_mib`。
- 每卡综合负载 = `0.5 × utilization_percent + 0.5 × memory_utilization_percent`。
- 节点平均负载 = 所有 GPU 的综合负载之和 / GPU 数量，每卡等权，不按显存容量加权。

例如两张卡的利用率/显存占用率分别为 80%/40%、20%/60%，每卡负载为 60%、40%，节点平均为 50%。这是平台定义的采样综合分数，不是 NVIDIA 官方指标，也不是过去一分钟等时间窗口平均。采集使用宿主机 `nvidia-smi`，应确保驱动工具可从 Agent 的 PATH 找到；systemd 服务重启后使用其实际环境。

驱动命令有 5 秒期限；型号优先从采集结果读取，否则保留 Incus 硬件信息。缺工具、超时、无效数据或不支持的厂商不会伪装成 0%：相应指标为 null，并说明错误；已有硬件信息仍返回 HTTP 200。任何卡负载未知时节点平均也为 null，避免只对正常卡求平均而低估节点负载。没有卡时 `gpus` 为 []，平均为 null。

这不是空闲/独占保证或显存硬配额。平均分可能掩盖单卡显存耗尽；调度仍需逐卡检查健康状态、剩余显存、已预留名额和业务额度。后端选择 UUID，Agent 在配置时重新解析当前节点 PCI。

### 5.3 `PUT /agent/v1/instances/{name}/gpu?project=default`

单卡请求体：

```json
{"gpu_uuids":["GPU-7f665bf9-5911-4f71-ac6a-a66e32362574"],"allow_live":false}
```

```http
PUT /agent/v1/instances/worker/gpu?project=default
Content-Type: application/json
```

| 字段 | 规则 |
| --- | --- |
| `gpu_uuids` | 必填数组；UUID 必须来自该节点；重复项、PCI 字符串、MIG 标识被拒绝；多个元素表示多卡 |
| `allow_live` | 可省略，默认 false；true 显式接受运行中改设备入口的风险 |

空数组移除 Agent 管理的 GPU 配置：

```json
{"gpu_uuids":[],"allow_live":true}
```

限制与结果：

- 仅管理本地 `agent-gpu-N` 设备；其他 GPU 配置、profile 中的 GPU 或保留设备名冲突会拒绝，不静默迁移现有手工配置。
- 集群中实例不在本节点时拒绝，后端应调用实例所在节点的 Agent。
- 默认只接受 `Stopped`，冻结、启动中等状态不当作关机；显式允许在线调整不代表所有实例/GPU 类型都支持热插拔。
- 单次更新内部期限 2 分钟，失败后最多再用 5 秒独立读取实际配置；客户端期限应留余量。
- 操作使用实例 ETag，但不能锁住其他 Incus 客户端、profile 修改或生命周期操作。

| 响应字段 | 含义 |
| --- | --- |
| `project` / `instance` | 请求目标 |
| `requested_gpu_uuids` | 请求的 UUID 列表 |
| `operation_id` | 已获得时返回；用于继续查询 `/1.0/operations/{id}` |
| `operation_completed` | 此次等待是否确认操作成功；false 不等于一定没有修改 |
| `configuration_state` | `observed` 或 `unknown` |
| `actual` / `etag` | 重新读取的实例对象及 ETag；读取失败可能没有 |
| `resource_release` | 始终为 `not_verified`，不是强制回收确认 |
| `warnings` | 资源回收、并发与热插拔限制 |
| `error` / `message` / `observation_error` | 操作失败或结果未知时的说明 |

**在线换卡不结束旧 CUDA context。** 旧任务可能继续占旧卡，新进程使用新卡；不能仅看配置就释放名额或结束记账。失败也不保证旧挂载已恢复。Agent 目前不提供强杀 GPU 进程、整卡 reset、显存硬配额或计费 API。

## 6. 实例常用接口

以下表格路径均以 `/1.0` 开头；项目参数按需附加。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/1.0` | 服务端版本、`api_extensions`、运行环境 |
| GET | `/1.0/instances?recursion=1` | 实例列表与详情 |
| POST | `/1.0/instances` | 创建、导入备份或迁移目标，具体由请求类型决定 |
| GET | `/1.0/instances/{name}` | 实例配置与 ETag |
| PATCH / PUT | `/1.0/instances/{name}` | 部分 / 全量更新可写配置 |
| POST | `/1.0/instances/{name}` | 重命名、移动或迁移，不是开关机 |
| DELETE | `/1.0/instances/{name}` | 删除实例，具有破坏性 |
| GET / PUT | `/1.0/instances/{name}/state` | 读取状态 / 启动、停止、重启等 |
| POST | `/1.0/instances/{name}/rebuild` | 重建根盘系统；需对应扩展且有数据丢失风险 |

### 6.1 创建示例

前提：本地已有镜像别名 `studio-ubuntu24-cloud`，已有第 8 节的 `studio-base` profile；镜像带 cloud-init。请求体：

```json
{
  "name":"worker",
  "type":"container",
  "profiles":["studio-base"],
  "config":{"limits.cpu":"2","limits.memory":"4GiB","security.privileged":"false"},
  "source":{"type":"image","alias":"studio-ubuntu24-cloud"},
  "start":false
}
```

```http
POST /1.0/instances?project=default
Content-Type: application/json
```

收到 202 后按第 4.3 节等待并检查终态。`type: virtual-machine` 是 VM，节点必须另具备 QEMU/KVM 等条件；Agent 不会自动安装这些依赖。

### 6.2 启停与重启

启停请求体：

```json
{"action":"start","timeout":30,"force":false,"stateful":false}
```

```http
PUT /1.0/instances/worker/state?project=default
Content-Type: application/json
```

将 action 改为 `stop` 或 `restart` 对应关机或重启。不要默认为强制停止；强制停止会终止任务。每次仍需查询异步结果与最终实例状态。

### 6.3 更新配置

先 GET 实例取得 ETag，请求体：

```json
{"config":{"limits.cpu":"4","limits.memory":"8GiB"}}
```

```http
PATCH /1.0/instances/worker?project=default
Content-Type: application/json
If-Match: "实际ETag"
```

不要提交示例中的文字 ETag。运行中能否修改某项配置由 Incus 决定；`nvidia.runtime` 等启动期配置不能靠在线换卡刷新。

## 7. 镜像、快照、备份与恢复

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET / POST | `/1.0/images` | 列出 / 导入、拉取、发布镜像 |
| GET / PUT / PATCH / DELETE | `/1.0/images/{fingerprint}` | 镜像信息、属性与删除 |
| GET / POST | `/1.0/images/aliases` | 镜像别名列表 / 创建 |
| GET / PUT / POST / DELETE | `/1.0/images/aliases/{name}` | 查询、更新、重命名、删除别名 |
| GET | `/1.0/images/{fingerprint}/export` | 下载镜像 |
| GET / POST | `/1.0/instances/{name}/snapshots` | 快照列表 / 创建 |
| GET / PUT / PATCH / POST / DELETE | `/1.0/instances/{name}/snapshots/{snapshot}` | 快照信息、属性、重命名/迁移和删除 |
| GET / POST | `/1.0/instances/{name}/backups` | 备份列表 / 创建 |
| GET / DELETE | `/1.0/instances/{name}/backups/{backup}` | 备份信息 / 删除 |
| GET | `/1.0/instances/{name}/backups/{backup}/export` | 下载备份字节流 |

创建镜像别名的 JSON 为 `{"name":"studio-ubuntu24-cloud","target":"实际镜像fingerprint"}`。远程镜像源、拉取凭据和网络条件按 Incus 原生 API 配置，不是上传 Docker 镜像即可作为系统容器。

快照创建 JSON 示例：`{"name":"before-upgrade","stateful":false}`；备份创建：`{"name":"daily","instance_only":true,"optimized_storage":false}`。它们都要等待异步操作完成。`instance_only` 不代表包含所有独立数据卷。

下载备份使用下列接口；大文件应设置充足的请求期限，响应为备份字节流：

```http
GET /1.0/instances/worker/backups/daily/export?project=default
```

后端必须检查 HTTP 状态并完整读取响应，失败或断流不能当作完整备份。快照恢复使用实例更新的 `restore` 字段；备份导入使用 `POST /1.0/instances` 的二进制导入协议。恢复前核对存储驱动、目标配置、数据卷和原实例状态。快照不是异机备份，GPU 任务也不会因此获得无损迁移能力。

## 8. 网络、IP 与端口映射

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET / POST | `/1.0/networks` | 网络列表 / 创建托管网络 |
| GET / PUT / PATCH / POST / DELETE | `/1.0/networks/{name}` | 配置、更新、重命名、删除 |
| GET | `/1.0/networks/{name}/leases` | DHCP 租约 |
| GET | `/1.0/networks/{name}/state` | 网络运行状态 |
| GET / POST | `/1.0/networks/{name}/forwards` | 转发列表 / 创建 |
| GET / PUT / PATCH / DELETE | `/1.0/networks/{name}/forwards/{listenAddress}` | 转发查询、修改、删除 |

**下面是新建托管 NAT 桥的独立示例，不是修改当前 macvlan 的步骤。** 先确认网段不与现有 LAN、VPN、路由冲突，也不要把宿主机物理网口随意加入桥。

创建网络的请求体：

```json
{"name":"studio0","type":"bridge","config":{"ipv4.address":"10.101.0.1/24","ipv4.nat":"true","ipv6.address":"none"}}
```

```http
POST /1.0/networks?project=default
Content-Type: application/json
```

创建 profile 的请求体，其中 `gpu-test-pool` 必须已存在；可换成正式池名：

```json
{
  "name":"studio-base",
  "config":{"nvidia.runtime":"true","nvidia.driver.capabilities":"compute,utility"},
  "devices":{
    "root":{"type":"disk","path":"/","pool":"gpu-test-pool"},
    "eth0":{"type":"nic","network":"studio0","name":"eth0"}
  }
}
```

```http
POST /1.0/profiles?project=default
Content-Type: application/json
```

此 GPU-ready profile 要求节点已准备 NVIDIA 运行库；纯 CPU 节点应使用没有 NVIDIA 配置的 profile。profile 不放 GPU 设备，由 UUID 接口按实例分配。

托管 bridge 下可通过实例 NIC 的 `ipv4.address` 配置保留 DHCP 地址，但仍要保证来宾会通过 DHCP 配置网卡。macvlan 不是这套 DHCP 管理模型：它仍需来宾静态配置或外部 DHCP；不要把其 NIC 填上该字段就当作分配成功。IP 归属、地址冲突和成员配额仍由后端管理。

端口映射请求体（目标 IP 替换为实例实际稳定地址）：

```json
{"listen_address":"192.168.1.99","ports":[{"protocol":"tcp","listen_port":"22022","target_address":"10.101.0.10","target_port":"22"}]}
```

```http
POST /1.0/networks/studio0/forwards?project=default
Content-Type: application/json
```

检查监听地址/端口可用、目标实例 SSH 正常、来源防火墙与回程路由。托管 bridge 的 forwards 还有防火墙驱动和 Incus 版本条件；不能套到 macvlan，也不自动配置上游路由器的公网 NAT。不要覆盖 Agent 管理端口或已有服务端口。

## 9. 存储、profile、项目与集群

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET / POST | `/1.0/storage-pools` | 存储池列表 / 创建 |
| GET / PUT / PATCH / DELETE | `/1.0/storage-pools/{pool}` | 池配置、更新、删除 |
| GET | `/1.0/storage-pools/{pool}/resources` | 池资源用量 |
| GET / POST | `/1.0/storage-pools/{pool}/volumes/custom` | 自定义卷列表 / 创建 |
| GET / PUT / PATCH / POST / DELETE | `/1.0/storage-pools/{pool}/volumes/custom/{volume}` | 卷配置、更新、移动/重命名、删除 |
| GET / POST | `/1.0/storage-pools/{pool}/volumes/custom/{volume}/snapshots` | 卷快照 |
| GET / POST | `/1.0/profiles` | profile 列表 / 创建 |
| GET / PUT / PATCH / POST / DELETE | `/1.0/profiles/{name}` | 查询、修改、重命名、删除 |
| GET / POST | `/1.0/projects` | 项目列表 / 创建 |
| GET / PUT / PATCH / POST / DELETE | `/1.0/projects/{name}` | 项目配置、修改、重命名、删除 |
| GET | `/1.0/projects/{name}/state` | 项目资源用量 |
| GET | `/1.0/cluster`、`/1.0/cluster/members` | 集群配置与成员；独立节点不等于已组集群 |

创建自定义卷 JSON 示例：`{"name":"user-data","type":"custom","content_type":"filesystem","config":{"size":"20GiB"}}`。先检查驱动是否支持配额；当前实验 `dir` 池不能因为请求有 size 就当作磁盘硬额度已生效。

把卷接入实例要更新其 devices，例如 `{"type":"disk","pool":"实际池名","source":"user-data","path":"/data"}`，并保留其他设备；不接受成员任意指定宿主机目录。持久卷的删除、快照、导出、搬迁与实例根盘是不同操作。

修改共享 profile 会影响引用它的实例，不等于只修改一个实例。项目隔离也不是业务用户权限系统；后端仍持有所有项目的管理凭据。

## 10. 文件、命令、Web 终端与事件

### 10.1 文件接口

| 方法 | 路径 | 请求/响应 |
| --- | --- | --- |
| GET | `/1.0/instances/{name}/files?path=...` | 下载文件或列目录；返回格式由文件类型决定 |
| POST | 同上 | 请求体是文件原始字节，不是 JSON/base64；通过 `X-Incus-*` 头指定属性 |
| DELETE | 同上 | 删除来宾内目标路径 |
| GET | `/1.0/instances/{name}/sftp` | HTTP Upgrade 到 SFTP；需要相应协议客户端 |

```http
GET /1.0/instances/worker/files?project=default&path=%2Fetc%2Fos-release
POST /1.0/instances/worker/files?project=default&path=%2Froot%2Fnote.txt
Content-Type: application/octet-stream
X-Incus-type: file
X-Incus-mode: 0600
X-Incus-uid: 0
X-Incus-gid: 0
```

文件接口中的 path 是**容器内路径**。后端应验证成员的实例归属、允许操作范围和上传大小。目录响应和普通文件正文不要混作同一 JSON 模型。

### 10.2 非交互命令

`POST /1.0/instances/{name}/exec`，请求体：

```json
{"command":["/usr/bin/id"],"interactive":false,"wait-for-websocket":false,"record-output":true}
```

```http
POST /1.0/instances/worker/exec?project=default
Content-Type: application/json
```

等待 operation 完成后读取操作 metadata 中的退出码和输出位置；通过返回的日志 URL 下载 stdout/stderr。命令返回非零退出码，不一定表示 HTTP 或 Incus operation 自身失败。不要擅自拼接 `sh -c`，传入参数数组更清晰。

### 10.3 交互终端与控制台

- 交互 shell：`POST /1.0/instances/{name}/exec`，例如 command 为 `/bin/bash`，`interactive:true`、`wait-for-websocket:true`，environment 指定 TERM。
- 系统控制台：`POST /1.0/instances/{name}/console`，按实例类型选择 console 协议；不是 exec shell。
- 按创建响应的 operation ID 与 `metadata.fds` 中的 secret，连接 `wss://节点/1.0/operations/{id}/websocket?secret=...`，携带相同 Token 并固定验证同一证书。
- 每个数据/控制通道如何连接、窗口大小和信号如何传递，遵循 Incus exec/console 协议。创建请求成功不等于终端通道已建立。

业务后端需实现 PTY、窗口缩放和多 WebSocket 通道对接。浏览器 WebSocket API 不能随意设置 Authorization，因此**浏览器应连接业务后端，由后端鉴权后代理 Agent 通道**；不能为方便而把节点 Token 放在 URL 或发给浏览器。

### 10.4 事件流

`GET /1.0/events?project=default&type=operation,lifecycle` 使用 **WebSocket**，不是普通 JSON 长轮询或 SSE。通过 WSS、Bearer 与同样的指纹验证连接。断开后重新查询任务/实例状态，不能认为断线期间没有事件。

## 11. 运维与排错

| 现象 | 检查与处理 |
| --- | --- |
| `init` 提示目录存在 | 换新的目录，不删除已有凭据重试；初始化不提供覆盖模式 |
| `-check` 成功但 health 503 | 检查 Incus 服务、socket 路径与权限；预检没有检查 Incus |
| 证书名称不匹配 | 使用导出的 endpoint；不要跳过主机名验证或改用未列入 SAN 的地址 |
| 指纹不匹配 | 停止自动连接，核实节点和证书变更，通过可信渠道重新导入 |
| HTTP 401 | 检查节点连接配置是否配错、Token 是否变更、是否重复加入认证头 |
| GPU 返回 409 | 查看具体 error；可能是运行中、已有手工 GPU/profile、实例不在本节点 |
| GPU 结果 unknown / 502 | 保存 operation ID，查询实际实例；不盲目再次分卡或释放旧名额 |
| 备份/事件连接被中断 | 检查调用方 context、前置代理超时及 Agent 是否重启 |
| Agent 重启后实例仍运行 | 正常；Agent 不管理用户实例的开机自启动策略 |

配置在启动时加载。换 Token 或证书后必须重启 Agent；旧 WebSocket 也会断开。证书默认一年，到期前应安排变更。

当前简单轮换方式：停止 Agent，以新目录重新初始化，更新服务配置并通过可信渠道重新导入连接文件后启动；这会同时更换证书和 Token，但不删除 Incus 实例。没有旧凭据兼容窗口、自动接受新指纹或无中断轮换。泄露的连接配置应按节点管理员凭据失陷处理。

## 12. 完整 API 与验证范围

本文常用原生路径和字段按 Incus v6.0.5 规范核对，不承诺列出所有 Incus 功能。实际节点以 `GET /1.0` 的版本和 `api_extensions` 为准。

- [v6.0.5 完整 REST OpenAPI 规范](https://github.com/lxc/incus/blob/v6.0.5/doc/rest-api.yaml)
- [Incus REST API 通用规则](https://linuxcontainers.org/incus/docs/main/rest-api/)
- [exec 协议说明（固定版本）](https://github.com/lxc/incus/blob/v6.0.5/doc/instance-exec.md)
- [项目 README](../README.md)

本文接口路径和请求字段已按 Incus 规范核对。本次未在真实服务器执行实例创建、NAT、备份恢复或 GPU 操作。所有生产变更须由管理员评估资源与数据风险；本项目不会因编写指南自行 SSH。

GPU 负载接口另已通过竞态测试及实际 Agent 进程烟测：用本地协议/驱动命令夹具验证 UUID 匹配、型号、每卡 60%/40% 与节点平均 50%，以及命令失败时保留硬件信息、返回 null 与错误。该验证不代表真实 NVIDIA 硬件采样；本次未 SSH。
