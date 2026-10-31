# 101cloud Backend

工作室实例管理后端，使用 Go、Gin、MySQL 和 GORM。提供 CPU 虚拟机、GPU 容器、积分计费、管理员自定义实例、监控和 Web 运维接口。

后端自身提供 **HTTP**，默认 `127.0.0.1:8080`。浏览器入口的 HTTPS 交给 Nginx/Caddy；Host Agent 仍使用独立的 HTTPS 和证书指纹认证。当前不提供 NAT 或端口映射。

## 1. 部署前准备

推荐使用 Linux 服务器，准备：

- MySQL 8.4 LTS，以及独立数据库和数据库用户。
- 构建环境 Go 1.25 或更新版本；也可以在其他机器构建后上传二进制。
- Nginx 或 Caddy，以及浏览器入口使用的域名和 HTTPS 证书。
- 已安装、初始化好的 Host Agent。后端不会通过 SSH 自动安装 Incus、驱动或修改宿主网络。

后端可以与节点部署在同一台机器，也可以单独部署。服务器之间必须能访问 Agent 的管理端口。

## 2. 构建程序

在完整仓库中构建，不能只复制 backend 源码：go.mod 引用了相邻的 host-agent 客户端包。

```bash
cd backend
go build -o bin/101cloud-backend ./cmd/backend
```

交叉构建 Linux amd64：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/101cloud-backend ./cmd/backend
```

ARM64 服务器将 `GOARCH` 改为 `arm64`。编译后的程序可以单独上传，不需要在目标服务器保留源码；SQL 迁移已经嵌入程序。

## 3. 创建运行用户和目录

以下以 Ubuntu/Debian、服务用户 `101cloud` 为例，需要管理员权限：

```bash
sudo useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin 101cloud
sudo install -d -m 0755 /usr/local/bin
sudo install -d -m 0750 -o root -g 101cloud /usr/local/etc/101cloud
sudo install -m 0755 bin/101cloud-backend /usr/local/bin/101cloud-backend
```

如果用户已经存在，不必重复创建。

部署路径统一为：二进制 `/usr/local/bin/101cloud-backend`，配置目录 `/usr/local/etc/101cloud/`。systemd服务单元仍放在系统标准目录 `/etc/systemd/system/`。

## 4. 准备MySQL服务，不需要手工建库建用户

首次部署只需MySQL 8.4已经安装、启动，并有可用于初始化的MySQL管理员连接。目标业务库可以尚不存在，也可以已经创建但完全没有表。

后面的 `install` 命令会自动创建数据库、执行全部建表迁移、创建业务MySQL用户，并授予该库的SELECT/INSERT/UPDATE/DELETE权限，再用业务用户验证连接。业务用户名、密码和库名取自CLOUD_MYSQL_DSN，不会自动生成或修改已有账号密码。

MySQL管理员需有建库、建表/迁移、创建账号及授权的权限，并能检查mysql.user。管理员凭据仅用于安装进程，不能放进长期运行的systemd环境文件。普通服务不使用root/DBA。

**install仅用于首次空库安装。** 库内已有任何表或同名业务账号已存在时，明确拒绝覆盖；失败留下的部分DDL需要检查后修复，不自动删库或删用户。已有安装升级使用migrate，而不是重复install。

## 5. 生成主密钥

只在首次部署生成一次：

```bash
sudo sh -c 'umask 077; openssl rand -base64 32 > /usr/local/etc/101cloud/master.key'
sudo chown 101cloud:101cloud /usr/local/etc/101cloud/master.key
sudo chmod 600 /usr/local/etc/101cloud/master.key
```

主密钥用于保护节点凭据等数据，文件必须为 `0600`，且服务用户可以读取。**与数据库一起安全备份，不要每次部署重新生成，也不要提交 Git。** 丢失后无法正常解密原有凭据。

## 6. 配置环境文件

创建 `/usr/local/etc/101cloud/backend.env`：

```dotenv
CLOUD_ORIGIN='https://cloud.example.com'
CLOUD_LISTEN='127.0.0.1:8080'
CLOUD_MYSQL_DSN='101cloud_app:替换为运行账户密码@tcp(127.0.0.1:3306)/101cloud'
CLOUD_MASTER_KEY_FILE='/usr/local/etc/101cloud/master.key'
CLOUD_NODE_ALLOW_CIDRS='192.168.0.111/32'
CLOUD_SESSION_TTL='12h'
CLOUD_CONSOLE_MAX_CONCURRENT='4'
CLOUD_TRUSTED_PROXIES='127.0.0.1/32,::1/128'
```

```bash
sudo chown root:root /usr/local/etc/101cloud/backend.env
sudo chmod 600 /usr/local/etc/101cloud/backend.env
```

- `CLOUD_ORIGIN` 是浏览器访问面板的外部 origin，格式 `https://host[:port]` 或 `http://host[:port]`：小写、不带默认端口（443/80）、不带路径、查询、末尾 `/` 或用户信息，必须与浏览器发送的 `Origin` 头逐字相同；不是 Agent 地址。生产必须用 `https://`。
- scheme 决定 Cookie：`https://` 使用 `__Host-101cloud_session`、`__Host-101cloud_csrf` 并带 Secure；`http://` 使用 `101cloud_session`、`101cloud_csrf` 且不带 Secure（浏览器不会在普通 http 下保存 Secure/`__Host-` Cookie）。两者都是 HttpOnly、Path=/、SameSite=Lax、无 Domain。http 下口令和 Cookie 明文传输，启动时会输出警告日志，只用于本地开发。
- 本地前端开发：Vite 开发服务器把 `/api/v1` 代理到 `http://127.0.0.1:8080`（需开启 WebSocket 代理），浏览器访问 `http://localhost:5555`，后端设置 `CLOUD_ORIGIN=http://localhost:5555`。代理不得改写浏览器发送的 `Origin` 头，否则修改请求和 WebSocket 会被 403 拒绝。
- `CLOUD_TRUSTED_PROXIES` 是可信反向代理的 IP 或 CIDR 列表（逗号分隔，IPv4/IPv6），非法条目会使启动失败。留空（默认）表示不信任任何代理，客户端 IP 取 TCP 对端地址。只有 TCP 对端属于该列表时，才采用 `X-Forwarded-For` 中的客户端地址（用于登录按 IP 限速和会话记录）；不读取 `X-Real-IP`。
- `CLOUD_NODE_ALLOW_CIDRS` 是允许接入的节点管理 IP 范围，按实际节点填写，尽量使用单 IP `/32`。
- 后端没有自动加载 `.env` 的逻辑，由 systemd 读取文件。
- 不需要配置后端 SSL 证书。生产 HTTPS 由反向代理终结，`CLOUD_ORIGIN` 填浏览器看到的 `https://` 地址。
- 程序自动配置 MySQL 连接的 UTC+8、时间解析和严格 SQL 模式。

## 7. 自动初始化数据库及首个面板管理员

先完成主密钥和backend.env配置，再在管理员Bash终端执行：

```bash
sudo -i
set -a
. /usr/local/etc/101cloud/backend.env
set +a

read -rsp 'MySQL管理员密码: ' MYSQL_ADMIN_PASSWORD; echo
CLOUD_MYSQL_ADMIN_DSN="root:${MYSQL_ADMIN_PASSWORD}@tcp(127.0.0.1:3306)/" \
  /usr/local/bin/101cloud-backend install
unset MYSQL_ADMIN_PASSWORD
```

这一步自动建库、建表和创建业务MySQL用户；backend.env里的CLOUD_MYSQL_DSN仍是业务账号，不要改成root连接。MySQL root若使用auth_socket认证，可将管理员与业务DSN都配置为同一个Unix socket端点，并在root终端执行。

本机TCP默认创建 `'业务用户名'@'127.0.0.1'`，Unix socket默认localhost。远程MySQL必须另外指定CLOUD_MYSQL_APP_HOST，值是**后端访问MySQL时的客户端来源主机/IP**，不是数据库服务器地址；避免随意使用%。管理员与业务DSN必须使用相同网络类型和服务器端点。

以后升级：备份后用受控管理员/迁移账户临时覆盖CLOUD_MYSQL_DSN执行 `101cloud-backend migrate`，不再创建账号。不应给日常业务用户加建表或授权权限。DDL会隐式提交，失败时先检查实际完成的语句。

不再手工创建面板管理员，也不需要配置管理员用户名或密码环境变量。首次运行 `serve`（或启动下面的systemd服务）时，程序在事务中自动创建随机管理员账号及高强度随机密码，提交后输出一条凭据日志：

```text
WARN initial administrator created ... username=admin-<随机值> password=<随机密码>
```

使用systemd时查看：

```bash
sudo journalctl -u 101cloud-backend --no-pager
```

保存首次凭据并在登录后修改密码。数据库只保存Argon2id密码哈希，审计中不保存明文；**启动日志按你的配置包含明文初始密码，必须限制journal、容器日志及日志采集平台的访问**。后续重启不重置密码或再次输出凭据；丢失首次日志不能通过重启找回密码。已有管理员的数据库升级后不会新建另一名管理员。

初始化状态持久化并以数据库行锁串行，并发首启只会有一个进程创建和输出。该凭据日志不受数据库事务原子保护，若恰在提交后、日志写出前崩溃，需由受信DBA走人工账号恢复，不应清空初始化标记以制造新的管理员。

## 8. 配置 systemd

创建 `/etc/systemd/system/101cloud-backend.service`：

```ini
[Unit]
Description=101cloud Backend
After=network.target

[Service]
Type=simple
User=101cloud
Group=101cloud
WorkingDirectory=/usr/local/etc/101cloud
EnvironmentFile=/usr/local/etc/101cloud/backend.env
ExecStart=/usr/local/bin/101cloud-backend serve
Restart=on-failure
RestartSec=3
TimeoutStopSec=30
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now 101cloud-backend
sudo systemctl status 101cloud-backend
sudo journalctl -u 101cloud-backend -f
```

服务不自动执行数据库迁移或导入节点；仅首次启动自动创建管理员并输出一次凭据日志。数据库不在本机时，服务启动依然要求数据库已可连接。

## 9. 配置 Nginx 反向代理

以下片段放进已有的 HTTPS server 中；域名、证书和前端静态文件配置由你的入口服务管理。示例与上文 `CLOUD_TRUSTED_PROXIES='127.0.0.1/32,::1/128'` 配套（Nginx 与后端同机，经回环地址转发）：

```nginx
location /api/v1/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;
    proxy_buffering off;
}
```

```bash
sudo nginx -t
sudo systemctl reload nginx
```

前端与 API 应使用同一个外部 origin。不要把内部 8080 端口无保护暴露到公网；只开放反代入口。

`X-Forwarded-For` 使用 `$remote_addr` 覆盖而不是 `$proxy_add_x_forwarded_for` 追加，这样客户端自带的该头不会传到后端。注意：信任某个地址段意味着该段内任何连接都能伪造客户端 IP。信任 `127.0.0.1/32,::1/128` 时，本机任意进程以及经 SSH 隧道（`ssh -L`）从回环地址连入 8080 的用户都能通过伪造 `X-Forwarded-For` 绕过按 IP 的登录限速（按账号的限速仍然有效）。只列出实际的代理地址；不需要时保持为空。

当前控制台上游预连接属于准备该会话的进程，推荐先单后端进程部署。多进程或多副本需要会话亲和，不能让控制台创建和 WebSocket 连接随机落到不同进程。

控制台没有固定时长上限，也不通过自动续期延长会话；`CLOUD_SESSION_TTL` 默认12h的登录有效期仍然适用。每2秒核对绑定登录、用户启用状态、实例权限和运行状态；注销、登录到期、用户禁用、权限撤销或实例停止会关闭连接。默认每用户最多4个 preparing/ready/active 会话，帧/输入限制、准备超时及网络心跳继续生效；网络断开或服务停止同样会关闭连接，不保证永久在线。

正常停服只关闭本进程持有的预连接并记录 closed。异常退出可能留下 ready/active 行；缺少持久化进程归属信息时不能把本进程看不到的连接当作其他进程已失效。此类记录仍占并发名额，可由原登录通过 DELETE 会话接口关闭；绑定登录到期/撤销或用户禁用后后台也会关闭记录。不要按控制台年龄清理；多副本场景不得无差别关闭其他进程的会话。

## 10. 检查是否启动成功

服务器本机：

```bash
curl -f http://127.0.0.1:8080/api/v1/health/live
curl -f http://127.0.0.1:8080/api/v1/health/ready
```

外部入口：

```bash
curl -f https://cloud.example.com/api/v1/health/ready
```

就绪接口成功不代表已完成业务配置，也不代表 GPU、KVM 或节点网络已验收。本仓库后端提供 API，不包含可直接打开的管理面板前端；接口调用和前端开发参见 [接口文档](后端接口文档.md) 与 [OpenAPI](openapi.json)。

## 11. 首次业务配置顺序

管理员登录需要先获取 `/api/v1/auth/csrf`，保存 Cookie，再提交登录请求。修改操作还要按接口文档携带 CSRF、幂等键及 If-Match。

建议顺序：

1. 创建并发布价格、GPU价格、额度、GPU空闲、GPU调度、监控六类政策，再完整绑定平台政策。不提供默认价格或默认业务额度。
2. 创建镜像和初始化模板（如需）。镜像实际必须满足目标节点的容器/VM、架构和驱动要求。
3. 从可信渠道导入 Agent endpoint、Token和证书指纹，执行只读探测，确认后启用节点。
4. 给 GPU 配置价格、共享名额和可用状态。共享名额不等于显存隔离或算力保证。GPU 的 `gpu_price_version_id` 填 gpu-pricing 政策版本 ID（即 `/api/v1/admin/policies/gpu-pricing/versions` 返回的 id）；API 中所有政策版本引用都使用该 ID。
5. 登记局域网 IP 池和地址（池名称全局唯一），绑定现有 bridge 或适用的 macvlan，验证后启用。没有 NAT 或端口映射。实例所在节点由所选网络绑定决定，直接下发自定义实例时须指定 node_id。
6. 发布套餐，创建用户并明确配置配额。新用户配额全部为0，自定义实例还需 max_resident_instances。VM 套餐只提供 CPU；容器套餐可设可选 GPU（gpu.count=0、gpu.model 非空且 allowed_overrides.gpu_count.max≥1），提供 GPU 的套餐必须绑定 gpu-pricing。用户创建弹性实例时始终可以用 gpu.count=0 不选 GPU。
7. 先受控验证一个实例的创建、登录、停止、磁盘保留、GPU观测和控制台，再开放给成员。

每日额度由后台任务处理，首次发放完成前账户可能尚未就绪。自定义实例的申请和沟通在平台外完成，平台只保留管理员 `POST /admin/custom-instances` 直接下发（请求字段仍为 `approved_config`）及已有实例纳管，不提供申请/审批页面或接口。自定义实例按admin_exempt记录，不受每日额度停机影响，仍受资源及max_resident_instances配额约束。当前OpenAPI包含142个操作。

额度耗尽默认600秒后强停且不删盘；真实强停可能丢失未保存内存和在途写入。停机不等于 GPU 使用权已经移除：尚未释放的 GPU 分配仍保留并按持有区间记录。需要释放时走显式 GPU 移除流程。

## 12. 更新、备份与排错

更新顺序：备份数据库和主密钥 → 停止后端 → 上传新二进制 → 用迁移账户执行 migrate → 启动服务 → 检查健康与日志。不要在不兼容的旧程序继续写库时升级结构。

自定义申请退役升级需在停服及备份后执行 `CLOUD_MYSQL_DSN='<迁移账户DSN>' /usr/local/bin/101cloud-backend migrate`。新增 `20261030_80_retire_custom_requests.sql` 删除custom_instance_requests、residency_requests和instances.custom_reason；不会删除实例、任务、用户或通用审计。首次install同样按顺序执行旧建表及前向删除迁移；不得修改已应用的init.sql或历史迁移。

移除控制台固定时限的升级迁移为 `20261031_90_console_no_fixed_expiry.sql`，仅删除 console_sessions.expires_at 及两个过期索引，新增 user_id/status 索引；既有 closed/failed/expired 历史状态不变，不重新打开过期会话。升级前停止全部后端进程；确认旧连接均已终止后，可在迁移窗口将遗留 preparing/ready/active 行更新为 closed、写入 closed_at、清空 secrets_ciphertext 并递增 revision，避免遗留记录占用并发名额。迁移本身不盲目关闭可能仍由其他进程持有的会话；旧迁移保持不变。

至少备份：数据库、master.key、backend.env、外部 HTTPS 入口配置；节点导出的原始连接文件也按管理员凭据保护。恢复时数据库与主密钥必须匹配。

| 问题 | 优先检查 |
|---|---|
| 服务启动即退出 | journalctl；数据库连接、主密钥权限、迁移是否完成 |
| 浏览器登录后仍401 | `CLOUD_ORIGIN` 是否与浏览器地址栏 origin 完全一致（scheme、主机、端口）、Cookie是否被保存；https origin 必须真的经 HTTPS 访问，不能用 http 页面配 https origin |
| 登录返回401 invalid_credentials | 用户名或密码错误，或账号已停用；与会话失效的 unauthenticated 不同 |
| 修改请求403 | Origin、CSRF；上下文到期重新获取/auth/csrf |
| 操作412/428 | 刷新详情获取ETag，再按接口提交If-Match |
| 创建409 | 额度、显式配额、节点容量、GPU名额、IP和政策版本 |
| 节点无法接入 | CIDR允许范围、网络可达、Token和固定指纹；不跳过证书校验 |
| 任务unknown | 查询原任务并让管理员核对，不重复创建或直接释放预留资源 |
| 删除任务unknown / gpu_reclamation_unverified | 原生删除已确认但 GPU 证据不足；检查 Agent 新字段、采集完整性及节点时间，等待删除后新样本再管理员对账，不重复 DELETE 或手工释放 IP/GPU |
| 控制台无法连接 | WebSocket反代、会话ready状态、Origin及进程亲和性 |

GPU 实例删除先确认原生消失，再最多等待约 95 秒的独立监控证据：`collection_started_at` 必须晚于删除确认，`collected_at` 新鲜度不超过 30 秒，库存完整、实例数组显式存在、目标 UUID/项目+名称全部缺席，且顶层 `host_quality.attribution_complete` 必须显式为 true。该 host_quality 保留节点归属完整性，不因其他停止实例的逐实例未知而误判，也不把未知上下文或设备引用当安全。旧 Agent 缺字段时保留全部预留；先升级 Agent，再对原任务核对。没有活动 GPU 分配不等待指标。通过后 IP/GPU、用量与配额终态原子提交；没有持久删除意图不能仅凭消失释放。详见接口文档第 9 节。

删除回归沿用固定证书 TLS 节点 fixture 与连接内 MySQL 临时表，需 `CLOUD_TEST_MYSQL_DSN` 指向可创建临时表的测试 MySQL（按既有约定使用已迁移测试库）；无该变量时 MySQL 用例跳过。用例覆盖删除前缓存到删除后新样本、过期 operation 404/无 ID 意图的对账不重放、重复处理、证据不足保留全部资源及无 GPU 跳过指标。正向等待约 1 秒/场景，负向用调用方 300ms 上下文测试取消，不等待生产 95 秒上限；不代表真实 GPU 硬件验收。

完整架构、配置边界和本地验收记录见 [后端设计方案](后端设计方案.md)。本地协议验收不替代真实 KVM 启动、多GPU驱动热调整及VGA硬件验收；上线前需在获准的测试节点验证。

## 13. 实际部署记录（192.168.0.111）

已部署二进制/usr/local/bin/101cloud-backend、配置/usr/local/etc/101cloud/backend.env及主密钥，systemd服务101cloud-backend已启用开机启动。使用独立业务库101cloud_backend（57张表）及101cloud_backend@localhost，仅授予该库SELECT/INSERT/UPDATE/DELETE；原101cloud库和历史指标验收库保留。

HTTP监听127.0.0.1:8080，就绪检查、随机管理员登录、真实Agent固定证书探测及节点硬件库存查询已通过。管理员用户名admin-ZpM4Bz_jZ26Hy3O4；密码仅从服务器首次启动journal读取，不写入文档。backend.env为root:root 0600，master.key为101cloud:101cloud 0600。

当前没有配置面板HTTPS反代或前端；CLOUD_ORIGIN暂为https://192.168.0.111，仅用作验收Origin配置，不代表该HTTPS地址已有面板可访问。未修改现有80端口服务、防火墙、网卡、路由、驱动或Agent配置。远程临时上传目录与验收SSH隧道已清理，后端/MySQL/Agent继续运行。

该节点实际枚举到1张RTX 2080 Ti。用户提供保留IP192.168.0.116后，先通过Host Agent在ens18执行ARP探测，返回no_conflict_detected、available=true、无回应；再通过后端登记地址并验证macvlan直连网络（/24，网关192.168.0.2），未复用NAT桥、改动宿主网卡或占用已有112/113地址。

真实验收容器backend-acceptance-116（业务ID1，Incus名cloud-386f33c547cbd58e33b88f12）配置1vCPU、1GiB内存、3GiB磁盘。创建、GPU绑定、WebSocket终端、退出码7、正常停机/启动、启动后116地址及磁盘文件保留、在线空闲GPU移除均已通过。限时真实CUDA负载采到1个归属进程、220MiB显存、近似SM95%，后台已写MySQL；验收时实例样本188条、GPU样本179条、各17条汇总，7个忙碌GPU点，父子关联孤立记录0。数量是验收快照，持续采集后可能增长。

联调修复了两个真实协议问题：CPU时间窗两个端点统一量化为微秒并据此计算独立窗口，不再拒绝Agent的纳秒精度；终端正确响应Incus空文本EOF握手并预先等待真实操作结果，不伪造退出码。修复后的后端已部署，配置、Agent与数据库结构未因此改变。

测试负载已停止，GPU分配确认released，测试容器最终Stopped，磁盘数据和116地址预留保留供后续检查。其他原有实例状态不变。节点管理状态已回到maintenance，GPU在后端标记disabled；当前只有显式的零价格验收政策，未配置成员正式价格、额度或套餐，不应直接开放自助创建。未完成真实多卡均衡（本机只有一张可识别GPU）及KVM/VGA验收。

后续按用户要求直接SSH到服务器、在本机HTTP调用API复测（未做换卡）：登录/CSRF、缺少或陈旧If-Match拒绝、同幂等键返回相同任务及异参409、维护态刷新节点库存、116容器启动、客机exec真实退出状态和文件读取、等待客机网络就绪、单卡在线绑定及nvidia-smi、最新指标入库、空闲在线移除GPU、正常停机均通过。复测快照实例样本556条、GPU样本182条、实例汇总48条，孤立GPU子记录0。测试后保持验收容器Stopped、GPU已释放、节点maintenance及GPU disabled，其他原有实例状态未变。未声称本轮重新执行CUDA负载或多卡均衡。

2026-10-30 契约修正部署：新增 `CLOUD_TRUSTED_PROXIES`（X-Forwarded-For）、`CLOUD_ORIGIN` 支持 http/https（Cookie 名称与 Secure 随协议变化）、密码错误独立返回 `invalid_credentials`、列表与详情字段类型统一、对外政策版本引用统一为 `policy_versions.id`、弹性实例可选择不带 GPU。本地临时 MySQL 8.4 上 `go test -race ./...`（含 MySQL 回归）与 `go vet ./...` 通过，并用真实 HTTP 进程（http Origin、可信代理 127.0.0.1）验证 27 项：HTTP Cookie 名与 Secure、两类 401、改密码失败会话保留、按转发 IP 分桶限流、政策/镜像/地址池/绑定列表字段类型、GPU 定价与额度账户 ID、可选 GPU 套餐发布、报价 0 张/1 张/超范围。服务器仅替换二进制（备份 `/usr/local/bin/101cloud-backend.bak-20261030172121`），`backend.env` 与数据库结构未改；部署后只读核对真实数据：列表字段为对象/数组/布尔，GPU `gpu_price_version_id`、`/catalog/gpu-options` 的 `id`、使用记录与 GPU 分配的版本 ID 均等于对应政策版本 `id`，HTTPS Origin 下仍为 `__Host-` Secure Cookie，错误密码返回 `invalid_credentials`。当前未配置 Nginx，`CLOUD_TRUSTED_PROXIES` 保持为空。
