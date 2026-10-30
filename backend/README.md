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
CLOUD_CONSOLE_TTL='10m'
CLOUD_CONSOLE_MAX_CONCURRENT='4'
```

```bash
sudo chown root:root /usr/local/etc/101cloud/backend.env
sudo chmod 600 /usr/local/etc/101cloud/backend.env
```

- `CLOUD_ORIGIN` 是浏览器访问面板的外部 HTTPS 地址，不带末尾 `/`，不是 Agent 地址。
- `CLOUD_NODE_ALLOW_CIDRS` 是允许接入的节点管理 IP 范围，按实际节点填写，尽量使用单 IP `/32`。
- 后端没有自动加载 `.env` 的逻辑，由 systemd 读取文件。
- 不需要配置后端 SSL 证书。Secure Cookie 要求浏览器侧通过 HTTPS 访问。
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

以下片段放进已有的 HTTPS server 中；域名、证书和前端静态文件配置由你的入口服务管理：

```nginx
location /api/v1/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;
    proxy_buffering off;
}
```

```bash
sudo nginx -t
sudo systemctl reload nginx
```

前端与 API 应使用同一个外部 origin。不要把内部 8080 端口无保护暴露到公网；只开放反代入口。当前后端不信任任意转发代理 IP 头，不要假定加上 X-Forwarded-For 就会作为可信客户端地址使用。

当前控制台上游预连接属于准备该会话的进程，推荐先单后端进程部署。多进程或多副本需要会话亲和，不能让控制台创建和 WebSocket 连接随机落到不同进程。

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
2. 创建可用区、镜像和初始化模板（如需）。镜像实际必须满足目标节点的容器/VM、架构和驱动要求。
3. 从可信渠道导入 Agent endpoint、Token和证书指纹，执行只读探测，确认后启用节点。
4. 给 GPU 配置价格、共享名额和可用状态。共享名额不等于显存隔离或算力保证。
5. 登记局域网 IP 池和地址，绑定现有 bridge 或适用的 macvlan，验证后启用。没有 NAT 或端口映射。
6. 发布套餐，创建用户并明确配置配额。新用户配额全部为0，自定义实例还需 max_resident_instances。
7. 先受控验证一个实例的创建、登录、停止、磁盘保留、GPU观测和控制台，再开放给成员。

每日额度由后台任务处理，首次发放完成前账户可能尚未就绪。自定义实例只有管理员下发，按 admin_exempt 记录，不受每日额度停机影响。

额度耗尽默认600秒后强停且不删盘；真实强停可能丢失未保存内存和在途写入。停机不等于 GPU 使用权已经移除：尚未释放的 GPU 分配仍保留并按持有区间记录。需要释放时走显式 GPU 移除流程。

## 12. 更新、备份与排错

更新顺序：备份数据库和主密钥 → 停止后端 → 上传新二进制 → 用迁移账户执行 migrate → 启动服务 → 检查健康与日志。不要在不兼容的旧程序继续写库时升级结构。

至少备份：数据库、master.key、backend.env、外部 HTTPS 入口配置；节点导出的原始连接文件也按管理员凭据保护。恢复时数据库与主密钥必须匹配。

| 问题 | 优先检查 |
|---|---|
| 服务启动即退出 | journalctl；数据库连接、主密钥权限、迁移是否完成 |
| 浏览器登录后仍401 | 是否HTTPS、域名是否同源、Cookie是否被保存；不要关闭Secure Cookie绕过 |
| 修改请求403 | Origin、CSRF；上下文到期重新获取/auth/csrf |
| 操作412/428 | 刷新详情获取ETag，再按接口提交If-Match |
| 创建409 | 额度、显式配额、节点容量、GPU名额、IP和政策版本 |
| 节点无法接入 | CIDR允许范围、网络可达、Token和固定指纹；不跳过证书校验 |
| 任务unknown | 查询原任务并让管理员核对，不重复创建或直接释放预留资源 |
| 控制台无法连接 | WebSocket反代、会话ready状态、Origin及进程亲和性 |

完整架构、配置边界和本地验收记录见 [后端设计方案](后端设计方案.md)。本地协议验收不替代真实 KVM 启动、多GPU驱动热调整及VGA硬件验收；上线前需在获准的测试节点验证。

## 13. 实际部署记录（192.168.0.111）

已部署二进制/usr/local/bin/101cloud-backend、配置/usr/local/etc/101cloud/backend.env及主密钥，systemd服务101cloud-backend已启用开机启动。使用独立业务库101cloud_backend（57张表）及101cloud_backend@localhost，仅授予该库SELECT/INSERT/UPDATE/DELETE；原101cloud库和历史指标验收库保留。

HTTP监听127.0.0.1:8080，就绪检查、随机管理员登录、真实Agent固定证书探测及节点硬件库存查询已通过。管理员用户名admin-ZpM4Bz_jZ26Hy3O4；密码仅从服务器首次启动journal读取，不写入文档。backend.env为root:root 0600，master.key为101cloud:101cloud 0600。

当前没有配置面板HTTPS反代或前端；CLOUD_ORIGIN暂为https://192.168.0.111，仅用作验收Origin配置，不代表该HTTPS地址已有面板可访问。未修改现有80端口服务、防火墙、网卡、路由、驱动或Agent配置。远程临时上传目录与验收SSH隧道已清理，后端/MySQL/Agent继续运行。

该节点实际枚举到1张RTX 2080 Ti。用户提供保留IP192.168.0.116后，先通过Host Agent在ens18执行ARP探测，返回no_conflict_detected、available=true、无回应；再通过后端登记地址并验证macvlan直连网络（/24，网关192.168.0.2），未复用NAT桥、改动宿主网卡或占用已有112/113地址。

真实验收容器backend-acceptance-116（业务ID1，Incus名cloud-386f33c547cbd58e33b88f12）配置1vCPU、1GiB内存、3GiB磁盘。创建、GPU绑定、WebSocket终端、退出码7、正常停机/启动、启动后116地址及磁盘文件保留、在线空闲GPU移除均已通过。限时真实CUDA负载采到1个归属进程、220MiB显存、近似SM95%，后台已写MySQL；验收时实例样本188条、GPU样本179条、各17条汇总，7个忙碌GPU点，父子关联孤立记录0。数量是验收快照，持续采集后可能增长。

联调修复了两个真实协议问题：CPU时间窗两个端点统一量化为微秒并据此计算独立窗口，不再拒绝Agent的纳秒精度；终端正确响应Incus空文本EOF握手并预先等待真实操作结果，不伪造退出码。修复后的后端已部署，配置、Agent与数据库结构未因此改变。

测试负载已停止，GPU分配确认released，测试容器最终Stopped，磁盘数据和116地址预留保留供后续检查。其他原有实例状态不变。节点管理状态已回到maintenance，GPU在后端标记disabled；当前只有显式的零价格验收政策，未配置成员正式价格、额度或套餐，不应直接开放自助创建。未完成真实多卡均衡（本机只有一张可识别GPU）及KVM/VGA验收。
