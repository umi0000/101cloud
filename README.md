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

### 面板
编译
```bash
cd backend
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ./build.sh -o bin/101cloud-panel
```
把编译产物传输到服务器，并添加config目录下的`101cloud-panel.service`到系统服务。
参考命令
```bash
scp backend/
```
