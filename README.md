# 101Cloud
101Cloud是一个多人容器/虚拟机管理面板，类似于vps厂商的那种面板，但是目前主要是用容器，然后也没有支付功能。\
容器解决方案是incus

主要面向工作室内多人自助调用显卡的场景。

## 架构
项目分为两个部分：面板和Host-agent
面板是实现业务的主逻辑（用户登录，实例管理，节点管理等）

面板后端用go编写，前端用vue3 + vite + antdesign。其实是套用了vben admin的框架）前端编译的静态文件被嵌入到了go的二进制文件里面，所以之需要一条命令即可启动前后端

Host-agent是一个操作器，部署在每一个节点上。一个面板可以连接多个节点。Host agent用于面板后端通过REST API调用incus。有人可能会说，incus有API直接提供啊，但是它没法返回节点的其他信息，如gpu的pcie地址，网卡信息和网段配置，IP可用性，节点硬件占用等。所以就需要一个host agent提供信息

## 部署（以下仅为示例）
首次构建：
从仓库根目录执行

```bash
npm install -g pnpm@10.33.4
cd frontend
pnpm install --frozen-lockfile
cd ../backend
go mod download
```
### 面板
编译(前端和后端会都编译，然后前端编译产物嵌入后端二进制文件)
```bash
cd backend
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ./build.sh -o bin/101cloud-panel
```
把编译产物传输到服务器，并添加config目录下的`101cloud-panel.service`到系统服务。
参考命令
```bash
scp backend/101cloud-panel root@xx.xx.xx.xx:/usr/local/bin
scp config/101cloud-panel.service root@xx.xx.xx.xx:/etc/systemd/system
scp config/config.env root@xx.xx.xx.xx:/usr/local/etc/101cloud-panel
systemctl daemon-reload
```
配置面板用户
```bash
sudo useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin 101cloud
```
安装数据库
```bash
sudo apt install mysql-server
```
生成主密钥

只在首次部署生成一次：

```bash
sudo sh -c 'umask 077; openssl rand -base64 32 > /usr/local/etc/101cloud-panel/master.key'
sudo chown 101cloud:101cloud /usr/local/etc/101cloud-panel/master.key
sudo chmod 600 /usr/local/etc/101cloud-panel/master.key
```

主密钥用于保护节点凭据等数据，文件必须为 `0600`

配置文件见配置文件模板

初始化：
不需要手动初始化数据库，通过程序会自动创建业务用户并建表
```bash
sudo 101cloud-panel install -c /usr/local/etc/101cloud-panel/config.env #通过socket使用数据库root连接数据库需要系统root用户执行
#systemd启动
systemctl start 101cloud-panel 
systemctl enable 101cloud-panel
#或者直接启动
101cloud-panel serve -c /usr/local/etc/101cloud-panel/config.env 
```
首次serve时会在日志中输出管理员账户密码
```bash
journalctl -u 101cloud-panel -f
```
进去找账号密码，然后就可以登录了

**补充**：面板二进制文件的cli命令
```text
101cloud-backend install [-c /path/to/backend.env] #首次安装，需要root权限
101cloud-backend migrate [-c /path/to/backend.env] #数据库迁移，需要root权限
101cloud-backend serve [-c /path/to/backend.env] #启动服务
```

### Host-agent
首先需要在节点机上安装incus
```bash
apt install incus
incus admin init
```

host agent也是用go写的
首先编译
```bash
cd host-agent
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/hostagent ./cmd/hostagent
```
传输到服务器上
```
scp host-agent/bin/hostagent root@xx.xx.xx.xx:/usr/local/bin
scp config/101cloud-agent.service root@xx.xx.xx.xx:/etc/systemd/system
systemctl daemon-reload
```
初始化hostagent
```bash
hostagent init --state-dir /usr/local/etc/101cloud-agent --endpoint https://127.0.0.1:8443
```
获取连接信息，用于在面板上添加节点
```bash
cat /usr/local/etc/101cloud-agent/connection.json
```
**启动**

```bash
#systemd启动
systemctl start 101cloud-agent
systemctl enable 101cloud-agent
#或者直接启动
hostagent -config /usr/local/etc/101cloud-agent/agent.json
```