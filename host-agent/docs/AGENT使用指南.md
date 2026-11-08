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

工作室正常业务默认使用局域网独立 IP，不默认采用 Incus NAT 子网。后端创建实例时应明确指定局域网网卡和首次启动前的静态地址；第 8 节 NAT 桥仅是可选网络示例。当前已验证的同网段 macvlan 配置见第 14 节。

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
| `network_interfaces` | 所有本机接口的地址、网段、默认路由及可确定归属的DNS，包括物理与虚拟接口 |
| `system_dns` | 系统级DNS、stub解析器、搜索域及来源；不是所有网卡DNS的混合列表 |
| `network_errors` | 网络枚举或默认路由采集的问题；部分失败不删除已获得的硬件/GPU/接口数据 |

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

#### 网卡网络信息

每个 `network_interfaces` 对象包含：

| 字段 | 含义 |
| --- | --- |
| `index` / `name` | 系统接口索引和名称，如ens18、incusbr0、VLAN、veth或lo |
| `mac` / `mtu` | MAC和MTU；loopback等接口MAC可为空 |
| `up` / `loopback` | 管理状态是否UP、是否回环，不等于外网可达 |
| `addresses[]` | 每个IPv4/IPv6的family、address、prefix_length、netmask、network |
| `default_routes[]` | 默认路由候选，含family、gateway、metric、table、weight；不擅自挑一个网关 |
| `dns` | 此接口的servers、source和可选error，不能将未知DNS当作空闲配置 |
| `error` | 可选，接口地址采集问题 |

示例仅展示新增网络部分，不是节点实时测量结果：

```json
{
  "network_interfaces": [
    {
      "index": 2,
      "name": "ens18",
      "mac": "bc:24:11:41:15:77",
      "mtu": 1500,
      "up": true,
      "loopback": false,
      "addresses": [
        {"family":"ipv4","address":"192.168.0.111","prefix_length":24,"netmask":"255.255.255.0","network":"192.168.0.0/24"}
      ],
      "default_routes": [
        {"family":"ipv4","gateway":"192.168.0.2","metric":100,"table":254,"weight":1}
      ],
      "dns":{"servers":["223.5.5.5"],"source":"systemd-resolved"}
    }
  ],
  "system_dns":{"servers":[],"stub_servers":[],"search_domains":[],"source":"systemd-resolved"},
  "network_errors":[]
}
```

接口地址取自本机真实配置，网段按地址前缀计算，不按IP地址类别猜掩码。Linux默认路由采集需要iproute2的`ip`命令，通过JSON读取IPv4/IPv6和路由表信息；命令执行受期限和输出上限约束。网卡可有多个地址和默认路由，ECMP下一跳按各接口分别返回；gateway为空可表示直连默认路由，不自动猜成网段中的`.1`。table用于区分main及策略路由表，weight是多路径权重；这些候选不构成对完整`ip rule`选路结果的保证。

DNS优先通过本机systemd-resolved的D-Bus查询，按接口索引关联，不解析本地化终端文本。resolved的全局index=0项放入`system_dns`，其他项只放对应接口；全局servers为空不代表所有接口没有DNS。resolved不可用时，系统级可后备读取resolv.conf；只有确认是resolved stub时才尝试其上游文件。loopback解析器仅列为stub_servers，不作为可直接给新容器使用的upstream；未能确定归属的接口DNS保持unavailable并给出error，不把全局DNS复制到所有网卡。

系统级DNS来源包括systemd-resolved、resolv.conf、systemd-resolved-resolv.conf或unavailable；需结合error判断后备/未知状态。resolved的search_domains仅包含全局非route-only域，不把其他网卡搜索域归给当前网卡。非Linux仍可返回Go可枚举的接口地址，但Linux路由/resolved能力不可用会明确报告，不伪装成空网关或配置成功。返回的是配置观察，不保证DNS可解析或链路可达。IPv6链路本地DNS若带`%接口名`，后端必须按目标容器接口重新解释scope，不能把宿主接口名照搬进去。

**供创建实例参考，而非自动生成完整套餐网络配置。** 后端先选定管理员允许的接口、网络模式和地址池；绝不能把宿主机address原样分给容器，不能从整个network网段推断所有地址可用。网关须结合表、metric、旁路代理和管理员策略选择；DNS缺失则要求明确配置。候选IP仍需数据库预留及第5.4节的冲突探测，且与外部DHCP地址池隔离。该信息接口不修改任何网卡、路由或DNS。

这些基本字段不等于复制完整的split DNS、DNS-over-TLS或策略路由规则，后端仍须使用管理员确认的网络模板。本次在本地实际运行HTTPS资源接口，验证了真实网卡地址、前缀/掩码计算和明确的平台限制；路由解析及文件DNS后备通过本地测试，Linux专属代码及测试已交叉编译。未进行新的Linux路由/resolved实机验收，未SSH、未启动远端测试容器，也未替换现有Agent。

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

### 5.4 `POST /agent/v1/network/ip-probe`

在**运行 Agent 的网络命名空间**内，通过指定网卡检测单个直连 IPv4 是否有 ARP 冲突。无需 Incus 在线；仍须 HTTPS 和节点 Token。接口不会分配IP、配置网卡、修改路由/邻居表、预留地址或扫描整个子网。

```http
POST /agent/v1/network/ip-probe
Content-Type: application/json
```

```json
{"interface":"ens18","address":"192.168.0.112"}
```

两个字段都必填：`interface` 必须是本节点实际可用的以太网接口；`address` 必须是该接口直连网段中的单个 IPv4，不接受域名、CIDR、IPv6、组播或网络/广播地址。点对点前缀的边界按其实际语义处理。请求体上限4 KiB，拒绝重复/未知字段、多JSON对象及额外查询参数，不允许调用方任意增加探测时长或次数。

| 字段 | 含义 |
| --- | --- |
| `interface` / `address` | 实际探测目标 |
| `status` | occupied / no_conflict_detected / unknown |
| `available` | 分别为 false / true / null；必须结合status，不把null转成true |
| `responders` | 冲突回应数组，每项为mac和source；MAC去重且有上限 |
| `reason` | 可选的结果或失败原因 |
| `warning` | 提醒本次结果不构成地址预留或DHCP安全保证 |
| `error` / `message` | HTTP错误时的机器代码和说明（参数解析错误仍用通用错误对象） |

发现本机已配置该地址时直接返回occupied；其他情况下在约3秒窗口内发送3次源IP为0.0.0.0的ARP探测，检测目标地址回应或其他设备对同一地址的并发探测。不会只因ping不通就认定空闲，也不依赖现有ARP缓存。它不是包含地址宣告和保留的完整地址分配协议。

业务结果occupied和no_conflict_detected返回200。平台不支持返回501；缺少原始套接字权限、链路无法确认或探测执行失败返回未知及相应错误；超时返回504。每个Agent最多4次并发探测，饱和时503 `probe_busy`，不无限排队。无效接口/地址返回400；错误和取消不会返回available=true。

**available=true只表示本次未发现冲突**，不是“可以无条件分配”：设备可能关机、不回复ARP，交换机/VLAN/代理ARP也影响结果，地址还可能已被DHCP或其他后端保留。选择VLAN时要指定正确的逻辑接口；通过路由访问的远端地址不适用本探测。

推荐后端先在数据库事务中预留候选IP → 调用Agent探测 → 无冲突后创建实例并核验地址生效；冲突则放弃该候选，未知则停止自动交付。必须同时将平台地址范围与外部DHCP池隔离。该接口不替代后端IPAM。

Linux实现不依赖arping命令，使用AF_PACKET原始套接字，需CAP_NET_RAW。Go网卡发现还需AF_NETLINK。随附systemd单元已允许这两类地址族；升级时要一起更新单元、daemon-reload并重启Agent，不能只换二进制。非root专用用户需要管理员授予相应能力；不要为此关闭安全模块或给用户开放宿主机命令执行接口。

### 5.5 容器级负载：`GET /agent/v1/instances/metrics`

```http
GET /agent/v1/instances/metrics?project=default
GET /agent/v1/instances/metrics?project=*
```

沿用节点HTTPS/Token认证。默认只返回default项目，`project=*`返回本节点全部项目，适合后端内部采集；不能直接将全部实例数据转给普通成员。Agent始终按本节点所有项目建立归属关系，过滤参数只影响公开输出，不把其他项目的进程误认为空闲。集群远端成员的PID不会当作本机PID。

这是后台连续采样缓存，不是请求来了才测一次。配置可省略，默认间隔5秒、每次采集期限4秒；也可在agent.json加入：

```json
{"metrics":{"interval_seconds":5,"timeout_seconds":4}}
```

interval_seconds范围1–60，timeout_seconds范围1–30；配置变化重启Agent生效。采样不并发重入，实际时间窗在响应中返回，不能假定每个点恰好相差5秒。采集是只读的，不会自动关机、释放、换卡，也不会将指标或进程明细写到文件。

| 顶层字段 | 含义 |
| --- | --- |
| `schema_version` | 当前为1，后端遇未知协议版本不能盲目入库 |
| `agent_boot_id` | 每次Agent启动随机生成的UUID，不是PID或宿主机标识 |
| `sample_sequence` | 本次启动内递增序号，后端用于去重 |
| `collected_at` | 快照时间，UTC；不等于每个底层读数原子同时发生 |
| `window_start_at` / `window_end_at` / `window_seconds` | 观察窗口，UTC微秒；首个窗口为0，不构造历史 |
| `collection_started_at` | 本次实例/设备输入采集开始前记录的UTC微秒时间；涵盖后续GPU采样，不是观察窗口起点 |
| `collection_duration_seconds` | 本次采集耗时，可用于评估时序偏差 |
| `collection_complete` / `collection_errors` | 实例枚举是否完整及安全错误码，不含进程路径/参数 |
| `stale` | 当前缓存是否已超出新鲜度期限；不能用于自动动作 |
| `instances` | 按项目过滤后的实例聚合列表 |
| `quality` | 全节点汇总质量；包括其他实例，不等于目标实例的独立质量 |
| `host_quality` | 实例行合并前的全节点证据质量：`complete`、`attribution_complete`、`context_attribution_complete`、`reasons`；不因无关Stopped实例缺PID而降级，不表示GPU空闲 |
| `anonymous` | 未归属上下文、设备引用、近期活动的匿名计数，不含进程标识 |

每个实例包含project、name、uuid、type、state、started_at，以及：

| 字段 | 含义 |
| --- | --- |
| `cpu.usage_ns` | 实例累计CPU时间，不能直接当百分比 |
| `cpu.allocated_ns_per_second` | 分配容量；1核对应每秒1e9纳秒 |
| `cpu.collected_at` / `cpu.window_seconds` | CPU计数器自己的观测时刻/差分窗，不受慢GPU调用污染 |
| `cpu.delta_ns` / `used_cores` | 有效窗内CPU增量及平均使用核数 |
| `cpu.allocation_percent` | 相对分配容量的使用率；首次、重启、计数倒退、缺测或跨规格变更时可为null |
| `memory.usage_bytes` / `limit_bytes` / `peak_bytes` | 内存用量、有效上限、运行生命周期峰值；未知不是0 |
| `gpus` | 配置与实际观察GPU的并集；会包含已不在配置中但仍被旧任务占用的卡 |
| `gpu_state` | in_use / idle_candidate / unknown / not_attached |
| `quality.complete` | 指标观察是否完整；缺失可选利用率也会使其为false |
| `quality.attribution_complete` | 上下文归属、设备引用及必要库存是否完整，独立于可选利用率是否有样本 |
| `quality.context_attribution_complete` | 显式上下文归属证据；仅稳定持有者的已知共享控制引用可与完整引用归属分离，未知引用/读失败/身份变化仍为false，不能替代释放门禁 |

每个gpus对象包含uuid、configured、observed、process_count、referencing_process_count、used_memory_bytes、sm_utilization_percent、utilization_method、collected_at、state和quality。process_count是去重的GPU进程数，不是CUDA context对象数量；与引用进程数不能相加。configured表示配置中确认的绑定，observed表示观察到实际占用或活动。观察不完整时false不能解释为确定没有占用，后端应结合quality保存未知。

GPU显存是NVML报告的归属进程显存合计；compute/graphics同PID去重，不作为显存硬配额或独占物理页计量。SM利用率是各归属进程最新有效样本之和并封顶100，方法名为`sum_latest_process_sm_percent_capped_100_approximate`，是近似活动指标，不是保证的算力份额。无样本/不支持/归属不明用null，不能伪装为0。

进程SM样本的身份起点是该PID/start/实例UUID/initStart连续可验证时的首次观测时间，不随每帧前移。NVML延迟返回的样本只有晚于该起点且仍在最大新鲜度内才可接纳；身份变化、退出或上下文消失后必须重新建立起点。不会缓存缺样本的SM或将不支持补为0。

NVML增量查询没有新SM记录不等于驱动失败。某实例在该卡上经验证无context，且查询明确空输出，或同卡存在其他已知持有者并确认利用率API可用时，使用现有 `gpu_utilization_no_samples`；SM仍为null，进程/引用/显存的真实0仍保留。不是把同卡其他实例的context或SM归给空闲实例。unsupported、无效记录、设备丢失及本实例有context但无有效SM样本仍为未知/失败原因；没有查询状态的空数组也不能自动视作正常无样本。停止实例没有进程根使用 `instance_not_running` 解释非运行态，不改其归因false和未知数量；运行实例根缺失仍使用 `instance_attribution_unknown`。这些原因同样保留质量complete=false，不能靠中性展示作为卸载授权。

可选指标缺失/陈旧/身份未确认等原因按卡生成，只向该卡所属实例汇总，不把其他卡或实例的警告反向合并进当前卡。顶层 `quality` 仍汇总所有实例；`host_quality` 保留采集时的全节点指标警告，但实例的公共基础质量只继承结构性不确定性。未知上下文/近期活动、未知设备引用、引用扫描失败、上下文库存不全、整次采样过期等仍全局fail-closed。已知共享控制引用仍使持有者full attribution=false，其已验证context/显存/SM可保留；同卡其他实例可独立具有完整空闲证据，不能据此放宽共享持有者的移除或换绑门禁。

**隐私：**Agent内部使用宿主PID、cgroup、启动标识和设备引用判断归属，公开API和历史表不含PID、命令行、进程名、可执行文件/用户文件路径。嵌套容器按外层Incus实例归属；进程退出、PID复用、权限不足、设备变化、MPS/MIG或无法可靠归属会保守降级。近期活动样本即使已无当前进程，也不能被当成空闲；无法可靠归属时增加匿名活动计数并阻止idle_candidate。Agent应作为宿主机服务运行；不能把普通容器内的/proc当作宿主PID视图。专用账户仅有Incus socket权限不代表能读取所有必要进程信息，缺权限会返回unknown，不会自行提权或放宽宿主安全策略。

#### 状态与自动化边界

- `in_use`：有明确上下文、设备引用或可归属活动证据，即使SM利用率为0也可能仍在使用。
- `idle_candidate`：本次完整观察未发现使用/引用，是候选，不是已经释放、也不是原子操作授权。可在SM缺样本时出现，但必须有完整的独立占用证据。
- `unknown`：信息缺失/陈旧/不完整，不能自动当作空闲。关机实例可能无法继续验证原运行进程身份，因此不强行报0。
- `not_attached`：在完整观察下没有配置绑定，也没有旧卡残留证据。

停止实例、换卡或重建后，不能把新旧CPU计数器拼接；跨分配容量变化的窗口不使用新容量错误归一化。内存peak_bytes是生命周期峰值，不是最近一分钟峰值。前端应分指标处理null和缺测，不能因GPU不支持某项指标而抹掉有效CPU/内存值，也不能把unknown画成0%。

上下文统计与设备引用完整性分开处理：已验证当前GPU进程归属时，process_count、used_memory_bytes及可用的近似SM值可以非null，同时quality.attribution_complete仍为false。可信rdev识别的共享nvidiactl/uvm引用可确认持有实例、但无法定位全部物理卡，使用gpu_shared_control_references，仅降低持有者及其卡的完整引用归属；referencing_process_count保持null，不计入anonymous.unresolved_gpu_references，不污染其他实例。持有者context_attribution_complete可保持true，shared-only也不能授权空闲释放；同时持有物理卡FD不能证明共享引用只作用于该卡。未知rdev、读取/限额失败仍保留全局gpu_reference_scan_incomplete/gpu_references_unresolved及context=false。容器边界取内核cgroup挂载根，兼容systemd的init.scope和同级system.slice，以及嵌套Docker不同PID namespace；PID/starttime、cgroup及实例根须跨扫描和NVML查询一致。设备basename不是物理卡身份。

`host_quality` 保留宿主进程扫描、上下文归属、FD设备引用、实例/设备库存、取消、新鲜度、MIG及匿名活动不确定性检查；它不放宽这些检查。与实例行一样，仅缺失可选利用率可使其 `complete=false` 而 `attribution_complete=true`。顶层原 `quality` 仍合并所有实例行，Stopped实例的 `instance_attribution_unknown` 不应被解释为宿主扫描失败；项目过滤只裁剪实例行，不重算这两个质量对象，也不改变 `collection_started_at`，历史samples使用同一字段。

原生删除后的资源回收必须先完成目标UUID/业务标记核验、持久化删除意图及原生缺席确认，再读取完整 `project=*` 实例列表，核对目标UUID及项目/名称确实缺席、所有行身份完整。快照必须 `stale=false`、不超过30秒、`collection_complete=true`，且 `collection_started_at` **严格晚于**原生删除确认时间；不能用发布于删除之后但实际采集始于删除之前的缓存。原有 `host_quality.attribution_complete` 显式true路径保留。另一个窄路径只允许宿主显式full=false/context=true、原因包含 `gpu_shared_control_references` 且其余仅为可选指标原因（`gpu_utilization_no_samples`、`gpu_utilization_unknown`、`gpu_metric_unavailable`、`gpu_utilization_stale`、`gpu_utilization_identity_unconfirmed`）；`anonymous.unattributed_contexts`、`unresolved_gpu_references`、`unattributed_activity_samples` 必须三个均显式数值0。完整实例列表还须包含至少一个非目标稳定共享持有者，其行明确context=true/full=false并有共享引用原因，不能含未知引用或结构性失败原因。缺字段、类型不符、未知原因、未知/孤儿上下文、近期未归属活动、引用失败或目标仍出现均不授权回收。不要以顶层汇总quality、GPU总显存为0或仅context=true代替上述证据；无关Stopped行可保持原有未知状态。该核对不是“整张物理卡空闲”，不跳过原生删除确认，也不把普通删除改走cleanup_stuck。

当前扫描边界必须保留：进程身份扫描覆盖有界宿主proc，但FD设备引用扫描仅对可归属于本次实例库存的进程执行；没有当前实例持有者的宿主进程会跳过FD扫描。三个匿名计数0只表示没有**已观察到**的未归属上下文、活动或未解析引用，不能证明不存在孤儿FD-only引用或所有内核资源已释放。已知其他实例的共享引用可以与目标删除分离；NVML仍报告的目标/孤儿上下文、残留近期活动、已扫描的未知引用及身份变化仍全局阻止回收。既有full=true和新增窄路径都不新增全宿主FD认证，不扩大扫描，也不特判驱动或Agent自身。

自动释放/空闲换卡的策略由后端决定，阈值、窗口和用户开关不在Agent写死。后端必须检查frame新鲜度、collection_complete以及**目标实例/目标卡**的attribution_complete和状态，而不是只看全节点汇总quality。连续候选也不能保证捕获所有采样间的短任务：实际动作前需串行化、核对当前配置/占用，动作后复核；旧卡残留就继续保留原分配名额，必要时走获准的停机回收。现有GPU配置API并未因该指标接口而变成原子“安全释放”事务。

纯新增挂卡与移除/换绑门禁不同：后端只有确认原生实际GPU集合是请求集合子集、身份/状态/库存及采样仍新鲜完整时，才可用显式context_attribution_complete替代目标的完整引用标志；旧Agent缺该字段不能盲目放宽。调用GPU配置接口必须附带add_only:true：Agent在锁和ETag内重新核对最新配置，只追加新设备，保留旧设备名和所有属性，拒绝隐式移除或未知物理映射。add_only默认false、仅接受JSON布尔值；运行实例仍需allow_live:true，profile/非托管设备冲突规则不变。仍存在实例的移除、重绑及空闲候选继续要求原有attribution_complete完整证据；已删除目标的回收仅适用上文独立的原生确认、时序、完整库存及已知其他持有者规则。

忙碌任务不做无感换卡。热插拔只改变设备入口，CUDA上下文、模型显存不会迁移；新程序或重新初始化后的程序才可使用新卡，已有应用还可能缓存设备信息。指标不用于按利用率收费，计费仍按配置/使用权持有时间。

#### 缓存与错误

采样器未启动/首点未完成返回503 `metrics_warming_up`。实例枚举不完整或最新快照过期时，latest返回503并保留快照/质量标记；不能把空列表当作所有实例已停止。底层GPU不支持但基本实例枚举成功时可以返回200及部分指标、null和质量说明。快照超过`2 × interval_seconds + timeout_seconds`视为stale。非Linux无法提供宿主Linux进程/GPU归属，明确降级，不模拟正常数据。

### 5.6 增量采集：`GET /agent/v1/instances/metrics/samples`

```http
GET /agent/v1/instances/metrics/samples?project=*&after_sequence=0&limit=32
```

后续请求带响应的agent_boot_id与next_sequence：

```http
GET /agent/v1/instances/metrics/samples?project=*&agent_boot_id=11111111-1111-4111-8111-111111111111&after_sequence=12&limit=32
```

UUID和序号仅示例，应替换为实际返回值。后端建议以project=*集中采集；按项目分别采集时必须使用独立游标。参数只允许project、agent_boot_id、after_sequence、limit；不允许重复参数，limit范围1–120。非零after_sequence必须带启动ID。

返回schema_version、agent_boot_id、oldest_available_sequence、newest_sequence、next_sequence、gap、stale和samples数组。每个sample与latest的快照字段一致，但新鲜度通过批次stale与每个点collected_at判断。Agent只保留120个不可变内存快照，默认约10分钟；没有本地磁盘历史库。它用于短断连补采，不代替MySQL。

启动ID变化返回409 `metrics_agent_restarted`，后端显式开启新一代游标，不能拼接CPU差分。序号超前返回400。所需历史已被覆盖时gap=true（首次请求0而序号1已丢失也如此），后端记录缺测范围，不能填0。缓存陈旧时返回503，不继续自动操作。重复读取同一快照使用同一个boot/sequence，不能按每次HTTP请求时间生成重复数据。

补采历史点按其原采集时间和当时质量入库，不因接收时已过去几分钟就篡改成0或新的当前点。相反，历史idle_candidate不能触发当前释放/换卡：自动动作必须再次取得目标实例的新鲜观察并核对当前状态，不能重放过去的候选事件。

### 5.7 MySQL长期历史与多粒度图表

新库使用 [初始化脚本](../../backend/sql/init.sql)。已存在原业务表的库使用 [指标增量升级](../../backend/sql/migrations/20261029_instance_metrics.sql)，先备份并选择正确数据库，只执行一次；不要对已有库重新运行整个初始化脚本。没有示例数据或默认管理员。

| 表 | 职责 |
| --- | --- |
| `instance_metric_samples` | 实例明细点、CPU累计/增量与独立时间窗、内存及质量 |
| `instance_gpu_metric_samples` | 每个点的每卡配置/观察标记、匿名数量、显存和近似利用率 |
| `instance_metric_rollups` | 按bucket_seconds汇总CPU/内存、有效时长、加权和与观测峰值 |
| `instance_gpu_metric_rollups` | 每卡对应粒度的有效时长、利用率/显存加权和及观测峰值 |
| `node_metric_cursors` | node+project_scope的启动ID/序号游标，与明细入库同事务推进 |

**边界：本次实现Agent采样与SQL结构，没有实现尚未开发的业务后端采集器、历史查询API、定时聚合或前端图表。** 后续后端负责：按实例UUID匹配业务归属 → 写入/去重原始点 → 同事务推进游标 → 依配置汇总、保留和清理。原始点唯一键包含node、agent_boot_id、sample_sequence和instance_id；不要只按实例名匹配删除后重建的实例。历史node是采样时位置，不复合绑定实例当前node，避免指标历史额外阻止迁移。

Agent接口时间仍为UTC；后端入库前统一转为UTC+8（+08:00），保留微秒精度，并确保数据库连接时区与驱动解析位置一致。已有DATETIME历史值不会因SET SESSION time_zone自动转换，不能直接混入新时区数据。CPU独立cpu_sampled_at/cpu_window_seconds不能用慢GPU的frame窗口代替；首次/重启没有CPU差分则为null。按CPU实际窗口计算CPU有效时长，按各自观察窗口切分到统计桶。覆盖时间取时间窗并集并裁到桶边界，不能把重叠窗口的秒数相加。支持按分钟、小时、天等配置bucket_seconds，但MySQL不自动生成汇总。

- 平均使用核数：有效CPU增量纳秒之和 / 1e9 / CPU有效秒数。
- 相对配置使用率的时间平均：cpu_utilization_pct_seconds / cpu_allocation_valid_seconds；如果选择容量加权，则用同一已知分配子集的cpu_normalized_usage_delta_ns / 1e9 / allocated_cpu_seconds × 100，不能混用不同有效子集。
- 内存与GPU显存平均：bytes×seconds加权和 / 对应有效秒数。内存百分比使用成对已知usage/limit的memory_utilization_pct_seconds / memory_utilization_valid_seconds，并保留观测峰值。缺失limit的区间不参与，不能把不同有效时间范围的使用量和上限直接相除。
- GPU SM平均：utilization_pct_seconds / utilization_valid_seconds；有效秒数0时返回null，不返回0。
- 更粗粒度要合并sum/valid_seconds与峰值，不能AVG(已经算过的平均值)。内存桶峰值取本桶内观测到的usage最大值，不能重复使用实例生命周期peak作为每小时峰值。采样间未捕获的瞬时峰值不能声称已测到。

进程统计只保留数量/峰值，不存原始PID或命令。取不到精细支持类别时保存unknown，不把null硬解释为unsupported或空闲。每个指标独立有效时长，coverage和unknown_count用于前端显示缺测。保留粒度/天数由后端配置；未配置清理会持续增长，不能误以为DDL自带保留任务。删除raw GPU子记录后再删raw实例点，汇总可以采用不同保留期。这里的历史是负载观测，不是pt计费账本。

### 5.8 固定远程镜像源与实际变体发现

所有接口仍需节点 HTTPS/Token 认证；这是节点发起的只读目录发现，不是平台直接抓取用户提供的 URL。

| GET 路径 | 响应 |
| --- | --- |
| `/agent/v1/image-sources` | `{"items":[{"id":"images","name":"Incus / Linux Containers","url":"https://images.linuxcontainers.org","protocol":"simplestreams"}]}` |
| `/agent/v1/image-sources/{source}/images` | `{"items":[RemoteImage,...]}`，空匹配为 `[]` |
| `/agent/v1/image-sources/{source}/images/{fingerprint}` | 单个 `RemoteImage`，不套 `items`；完整64位十六进制指纹不存在时404 |

`RemoteImage` 字段：`fingerprint`（完整64位）、`os_name`、`os_release`、`architecture`（Incus 原生名称）、`instance_type`（`container` / `virtual-machine`）、`variant`（源实际值，未提供时空字符串）、`cloud_init`（布尔）、`description`、`aliases`（字符串数组）、`size_bytes`（无精度损失的十进制字符串）。Linux Containers 源只有实际 `cloud` 变体标记为支持 cloud-init；`default` 不自动升级为 `cloud`。Ubuntu 等 OS 同样从该源的实际目录发现。Canonical Ubuntu Cloud 的旧 LXD 元数据不受 SDK v6.0.5 支持，因此不列为可选源，`ubuntu` 源 id 返回404；不重写格式、摘要或假称支持该来源下载。

列表查询可选 `os`、`release`、`architecture`、`instance_type`、`cloud_init`，精确匹配实际目录；`cloud_init` 仅接受 `true` / `false`，架构例如 `x86_64` / `aarch64`，不接受 `amd64` / `arm64` 别名。未知参数、重复参数、空值/非法值返回400；源列表及指纹解析不接受查询参数。固定源之外的 id 返回404，没有动态添加源 URL 的接口。

```http
GET /agent/v1/image-sources/images/images?architecture=aarch64&instance_type=container&cloud_init=true
```

每次刷新通过已安装的 Incus v6.0.5 客户端读取 SimpleStreams，期限30秒、最多累计64MiB响应与32次请求、最多50000个镜像记录。仅允许所选 HTTPS 源的路径；不跟随重定向、不使用环境代理、不允许调用者替换 URL。按源独立串行刷新，实际成功目录最多缓存2分钟，不在刷新失败时返回过期缓存。底层连接/解析/限额失败返回502 `image_catalog_failed`，超时/取消返回504 `image_catalog_timeout`，无 SDK 支持的镜像或关键元数据缺失返回422 `image_catalog_unsupported`。错误不透出上游响应原文。特别地，SDK v6.0.5 仅识别 `incus.tar.xz` / `incus_combined.tar.gz` 元数据；若某源只提供旧 `lxd.tar.xz` 等格式，明确报告不支持，不重写产品格式或伪造 OS/架构/变体后备条目。

发现与解析无本地 Incus 在线依赖，不代表镜像已下载。后端按节点、项目和实际指纹编排下述原生下载，保留每个节点独立任务；不得因另一节点成功就标记本节点可用。

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

固定远程源下载使用已发现的完整指纹与源列表中的 URL。以 `images` 源为例，向 `POST /1.0/images?project=default` 发送（替换占位指纹，不能提交占位值）：

```json
{
  "source": {
    "type": "image",
    "mode": "pull",
    "server": "https://images.linuxcontainers.org",
    "protocol": "simplestreams",
    "fingerprint": "<从实际目录选中的64位完整指纹>"
  },
  "public": false
}
```

原生代理原样返回异步 operation 引用，保存项目和 operation ID 后轮询原生操作/进度；最终以同项目 `GET /1.0/images/{fingerprint}` 核对确切指纹、架构与容器/虚拟机类型后才标记该节点镜像就绪。请求中断不证明下载失败，不得盲目重复派发。本地登记仅做该原生 GET 核验，缺失时明确失败而不偷偷联网拉取。镜像下载不创建/停止/修改实例，不修改共享原生镜像属性；GPU-ready 是业务目录的管理员断言，不是自动安装驱动或 CUDA 的承诺。

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

上述指南编写时只核对协议和本地测试；后续经用户授权进行的真实节点验收见第 13 节。生产变更须由管理员评估资源与数据风险，未经授权不 SSH。

GPU 负载接口另已通过竞态测试及实际 Agent 进程烟测：用本地协议/驱动命令夹具验证 UUID 匹配、型号、每卡 60%/40% 与节点平均 50%，以及命令失败时保留硬件信息、返回 null 与错误。该验证不代表真实 NVIDIA 硬件采样；本次未 SSH。

## 13. 真实节点验收：192.168.0.111

2026-10-28 经用户授权部署。宿主机 Ubuntu 26.04、Incus 6.0.5、x86_64；Agent 安装于 `/usr/local/bin/hostagent`，状态目录 `/etc/101cloud-agent`，systemd 服务 `101cloud-agent` 已启用，HTTPS 地址 `https://192.168.0.111:8443`。连接材料仅保存在节点受保护的 `connection.json`，不在文档记录 Token。

SSH 用于部署 Agent、检查宿主机前提及只读诊断；所有实例、镜像、存储 API 和容器内 exec 均通过认证 Host Agent，没有使用 Incus CLI 或绕过 Agent 直连 Incus socket。

| 项目 | 实测结果 |
| --- | --- |
| 实例 | `agent-ubuntu24-gpu-test`，default 项目，非特权，当前保留运行 |
| 系统 | Ubuntu 24.04.5 LTS cloud 镜像；cloud-init done、无错误 |
| CPU | 配置 4 核；进程 CPU affinity 数量实测为 4 |
| 内存 | cgroup memory.max = 4294967296 字节（4 GiB） |
| 根盘 | 既有 Btrfs default 池，API 实测 quota total = 8589934592 字节（8 GiB） |
| 配额边界 | 1 MiB 分配成功；在已有数据基础上申请 8 GiB 返回 EDQUOT；测试文件已清理 |
| GPU | RTX 2080 Ti，UUID `GPU-10877bdd-fb45-d0d1-6014-5d0b1d81e4a4`，由 UUID 接口配置 |
| CUDA | 实际分配内存并启动 sm_75 内核；1024 个结果全部为 130，PASS；容器可用 CUDA GPU 数量为 1 |
| systemd | running，失败服务 0；容器重启后 systemd 与 CUDA 再次通过 |
| 网络 | incusbr0 DHCP 地址 10.218.143.23/24，网关/DNS 与宿主 Agent 可达；外部 TCP 超时，待处理转发策略 |
| 既有实例 | `test` 仍在运行，未更改其配置 |

重启 operation 成功只表示容器已启动，不表示 D-Bus 就绪。验收中曾立即执行 systemctl 得到 bus 不存在，随后等待 `/run/systemd/private` 出现并等待系统就绪，再验证为 running；后端应有服务就绪阶段。

本机使用旁路代理与 Fake-IP，默认网关是 192.168.0.2；DNS 返回 198.18.x.x 属预期。只读诊断另发现 Docker 设置 IPv4 FORWARD policy DROP，现有链只放行 Docker bridge，没有 incusbr0 的新出站规则；Incus 独立 nftables 链中的 accept 不能覆盖另一条基础链的 DROP。目前未修改 DNS、网关、旁路代理或防火墙；出网修复需单独确认最小放行策略。不得把 Fake-IP 当成 DNS 配错而切公共 DNS 绕过代理。

用户随后明确选择“先不改防火墙”。本次验收保留该出网限制，不添加转发规则、不修改旁路代理；Agent 和测试实例保留运行，其他资源规格、systemd 与 CUDA 项均已通过。

## 14. 局域网独立IP实例：192.168.0.112

2026-10-28 按用户要求新建 `agent-ubuntu24-gpu-lan112`，不替换第 13 节的 NAT 测试容器。创建前从 ens18 发出 3 次 ARP 探测，未收到地址占用回应；这不等于已经在 DHCP 服务器配置保留，长期使用须确保该地址不被动态分配。

| 项目 | 配置及实测 |
| --- | --- |
| 地址 | 192.168.0.112/24，cloud-init 首次启动前传入静态配置 |
| NIC | macvlan，parent=ens18，name=eth0；不使用 incusbr0 |
| 网关 / DNS | 192.168.0.2 / 223.5.5.5，沿用节点旁路网络 |
| 规格 | Ubuntu 24.04.5 cloud，非特权，4核、4 GiB、Btrfs根盘配额8 GiB |
| 资源限制 | CPU affinity=4、memory.max=4294967296、卷 quota total=8589934592；超额分配返回EDQUOT，测试文件已清理 |
| GPU | UUID接口绑定RTX 2080 Ti；与原测试容器可共享同一物理卡，不代表独占或显存分区 |
| 系统 | cloud-init done无错误；systemd running、失败服务0 |
| 计算 | 真实CUDA内核1024个结果全部为130，PASS |
| 局域网 | 工作站ping 3次全部成功 |
| 出网 | archive.ubuntu.com解析到Fake-IP 198.18.0.54，Ubuntu镜像站和百度HTTPS均返回200 |
| 重启 | 通过Agent重启后，静态地址、网关、systemd和CUDA复验通过 |

所有实例创建、GPU分配、重启和容器内测试均经 Host Agent。没有修改宿主机网口、路由、Docker防火墙或旁路代理，也没有使用Incus CLI。既有 `test` 和 `agent-ubuntu24-gpu-test` 仍运行。

macvlan 默认隔离父宿主机与子接口，不能以宿主机直连容器失败判定局域网不可达；本次使用独立工作站验证。当前宿主机是虚拟机，已实际验证该环境允许此额外MAC，不据此保证其他上层虚拟交换机也允许。

## 15. 新实例首次APT对照：192.168.0.113

另经用户要求通过 Agent 新建 `agent-ubuntu24-gpu-lan113`，同一 Ubuntu24 cloud 镜像、4核、4 GiB、Btrfs 8 GiB硬配额、RTX 2080 Ti、systemd running、macvlan ens18，静态地址192.168.0.113/24。创建前ARP探测无冲突，既有容器未改动。

没有预先更新索引或修改源、代理、APT超时/重试参数。使用登录Shell执行 `apt update`，首次下载10.0 MB、约8.19秒完成，退出码0，保持镜像默认archive.ubuntu.com/security.ubuntu.com源，不是112上用户配置的清华源。外层90秒期限未触发；该次Agent exec未分配PTY，不代表已覆盖用户实际终端通道。CPU亲和性4、内存cgroup4 GiB、8 GiB超额EDQUOT及真实CUDA1024个结果全部通过。容器保留运行；这个成功结果不能解释112用户终端的历史卡点。

## 16. 真实SSH下APT挂起：已确认的终端差异

随后按用户授权直接密码SSH到192.168.0.113，复现默认 `apt update` 在下载前挂起。进程链为 apt → Ubuntu Pro的APT Pre-Invoke钩子 → `systemctl start --no-block apt-news.service esm-cache.service` → `systemd-tty-ask-password-agent --watch`。内核日志与PID核对确认systemctl发送TERM被该容器AppArmor策略拒绝；父进程停在do_wait，密码辅助进程停在poll。

仅从宿主机结束自有诊断产生的密码辅助进程，原apt立刻继续并退出0，验证了阻塞链条。无PTY的Agent exec不会触发同样的终端密码代理路径，因此之前Agent执行成功不能证明SSH正常；不能把原因归咎于下载40 MB索引、Fake-IP或软件源。

用户先授权临时设置 `kernel.apparmor_restrict_unprivileged_unconfined=0`，仅重新SSH无效，因此恢复1。再次获准后临时设0，并通过Host Agent仅重启113以重建现有上下文。之后真实SSH、分配PTY、不加APT特殊参数的 `apt update` 在2.88秒内退出0，无需手工结束helper；重启后的真实CUDA计算仍PASS。112及其他容器没有重启，不能声称其SSH问题已修复。

当前宿主机该运行时参数为0，未写sysctl持久配置；没有关闭整个AppArmor，未修改 `kernel.apparmor_restrict_unprivileged_userns`，未禁用APT沙箱或删除Ubuntu Pro钩子，也未换源、改DNS或代理。宿主机重启后参数可能恢复，长期策略须单独决定。Ubuntu24来宾并非在所有Ubuntu26宿主机交互路径都免受该限制影响；正式验收必须包含真实SSH/PTY，而不能只用非交互exec。

## 17. 容器指标验收范围

此前只读验收通过本地竞态/静态检查、Linux amd64/arm64无CGO构建以及Linux上的监控、NVML reader和node测试。覆盖共享卡独立归属、旧卡残留、PID复用/退出、短进程及非SM活动、CPU重启/容量变化/观测时间、缺失数据、隐私和缓存游标；当时只读检查CPU/内存及GPU空闲候选，未启动GPU负载。

同时修复Linux回归暴露的resolved D-Bus元组解码问题，以及旧Incus报告未创建设备的可选DRM control节点导致库存永远不完整的问题。权限错误仍明确未知，不以跳过错误假装完整。

此前MySQL 8.4.11临时库通过37表全新初始化、已有32表增量升级、采样幂等/快照外键/缺少CPU窗口/零有效时长不可伪造均值等约束验证，事务回滚并删除该临时库。

2026-10-29后续实测在192.168.0.111升级正式Agent并保留原凭据，复用MySQL、初始化空业务库。通过Agent创建隔离GPU容器、运行限时CUDA负载，修复真实systemd兄弟cgroup归属及通用控制引用抹掉已知统计的问题。最终部署版本实测进程数1、显存220MiB、近似SM利用率95%；停止负载后为idle_candidate且进程数/显存均为0，最后停止了测试容器，原实例状态不变。共享GPU同时繁忙仍由回归夹具覆盖，不宣称本次已实测多个容器并发负载。

临时采集任务在隔离库101cloud_metrics_accept_fe25c31b保留150条实例样本、150条GPU样本、各1条日报汇总和1条游标。实际重放批次验证去重，保留12条进程数NULL，汇总排除缺测的有效时长分母；3个Agent启动流保留独立幂等身份。业务库101cloud仅有37张空表，未混入验收数据。测试程序已清理，Agent与MySQL继续运行；没有长期后端采集服务，历史库不会自动更新。升级备份在节点/var/lib/101cloud-agent-backups/fe25c31b。详细接口与入库分工见第5.5–5.7节。
