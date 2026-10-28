# Podman + CDI：单卡、静态局域网 IP、root SSH 容器

## 先看结果

| 项目 | 当前部署 |
| --- | --- |
| 宿主机 | `root@192.168.1.99`，Ubuntu 26.04.1 |
| 运行时 | rootful Podman 5.7.0，crun；未启用 privileged |
| NVIDIA 工具 | 已有容器库 1.20.1；新增 `nvidia-container-toolkit-base=1.20.1-1` |
| 容器名 | `podman-ubuntu24-gpu` |
| 容器系统 | Ubuntu 24.04 应用容器，**PID 1 是 sshd，没有运行 systemd** |
| 业务 IP | **192.168.1.152/24**，网关 `192.168.1.1` |
| 固定 MAC | `b6:76:12:0a:6d:0b` |
| 显卡 | 一张 TITAN X Pascal；UUID `GPU-f5f4cd05-de43-485a-10ef-8e952f3d292b` |
| 资源 | 2 CPU、4 GiB 内存、512 进程上限；未设置磁盘硬配额 |
| 登录 | `ssh root@192.168.1.152`，密码按用户要求设置，不写入镜像或本文 |
| 自动重启 | `--restart=no`，没有配置宿主机开机自动启动容器 |
| 原有 Incus | `ubuntu24-gpu` 仍运行于 `.150`，没有修改或启停 |

实测密码 SSH 登录成功、容器 root UID 为 0；真实 CUDA 内核处理 1024 个整数，结果全部正确。固定 MAC 后再次重启容器，IP、MAC、SSH 主机密钥、密码登录与 CUDA 均保持正常。

**本机容器和地址已经存在，以下是人工复现教程，不要原样重复创建。新容器必须换名字、已预留的 IP 和唯一 MAC。**

## 1. 宿主机装什么？

前提是 NVIDIA 驱动已安装，`nvidia-smi -L` 正常。本次没有升级驱动或内核，没有重启宿主机。

在 Ubuntu 宿主机执行：

```bash
apt-cache policy podman nvidia-container-toolkit-base
apt-get -s install --no-install-recommends podman nvidia-container-toolkit-base
```

检查安装计划没有不明升级、删除或驱动变化后：

```bash
apt install --no-install-recommends podman nvidia-container-toolkit-base
podman version
nvidia-ctk --version
```

本机使用已配置的 NVIDIA 官方签名源。新节点没有该源时，先按 [Incus 部署文档中的“人工配置 NVIDIA 官方软件源”](INCUS_UBUNTU24_GPU_SETUP.md) 配置公钥与 `signed-by` 源；这里只复用软件源步骤，不需要安装 Incus。

本次验证版本是 Podman 5.7.0、Toolkit Base 1.20.1-1。新节点候选版本不同需要重新验收，不默认任何最新版都兼容。无需 Docker，不需要把宿主机驱动复制进镜像。

### 本次 NVIDIA 包下载失败的处理

节点直连 NVIDIA 仓库出现超时和 APT `Could not wait for server fd`。没有改全局 DNS、内核参数或使用不明软件源。

实际从工作站下载官方 `.deb`，用**已经由 APT 签名索引提供的 SHA256** 核验后传入节点 APT 缓存，再执行安装。该版本的实测值：

```text
URL: https://nvidia.github.io/libnvidia-container/stable/deb/amd64/nvidia-container-toolkit-base_1.20.1-1_amd64.deb
SHA256: e01964a064fbdb62425ab22ec5832349f92c5b0b50c8f23fdf14dad004db4fb2
```

人工遇到同样问题，可先在节点用 `apt-cache show nvidia-container-toolkit-base=1.20.1-1` 核对包与校验值，再在能访问官方站点的工作站下载、核验，通过 SCP 传到节点的 `/var/cache/apt/archives/`，然后安装。其他版本不能沿用这个哈希。

## 2. 配置 Podman 镜像源

这是 Podman 配置，不是 `/etc/docker/daemon.json`。在宿主机创建独立文件；已有同名文件先核对，不覆盖：

```bash
install -d /etc/containers/registries.conf.d
(set -o noclobber; cat > /etc/containers/registries.conf.d/50-dockerhub-mirror.conf <<'EOF'
[[registry]]
prefix = "docker.io"
location = "docker.io"

[[registry.mirror]]
location = "docker.m.daocloud.io"
insecure = false
EOF
)
```

采用 DaoCloud 的 Docker Hub 加速，保留 HTTPS 校验，不改全局 DNS。配置由之后的 Podman 命令读取，不需要重启一个 Docker daemon。

本次基础镜像 digest 已通过工作站直接查询 Docker 官方 registry 核对。先拉取确定的内容：

```bash
podman pull docker.io/library/ubuntu@sha256:78d4744ead1f5b19b04d0df558ab182a5300903900755e515b2ca4d10eb2044d
```

实测日志显示通过 `docker.m.daocloud.io` 拉取成功。该 digest 是本次 Ubuntu 24.04 amd64 镜像，不是永远更新的标签。固定基础镜像不等于冻结之后 apt 安装的所有软件版本。

## 3. 生成/检查 CDI：指定 UUID，不用 all

本次 Toolkit Base 安装时启用了 `nvidia-cdi-refresh`，已经自动生成 CDI 描述。人工检查：

```bash
systemctl status nvidia-cdi-refresh.service --no-pager
nvidia-ctk cdi list
```

如需要重新生成：

```bash
systemctl restart nvidia-cdi-refresh.service
nvidia-ctk cdi list
```

不另在 `/etc/cdi` 复制一份同名描述，避免重复定义。驱动、设备变化或重启后需要确保 CDI 刷新正常，不能永远使用旧描述。

本次使用的单卡名称：

```text
nvidia.com/gpu=GPU-f5f4cd05-de43-485a-10ef-8e952f3d292b
```

这是 Podman 原生 CDI，不设置旧式 `NVIDIA_VISIBLE_DEVICES=all`，也不开放 `--privileged`。CDI 不负责每用户卡数额度、显存分片或计费，这些不是本次试验已经实现的功能。

## 4. 创建不运行 systemd 的 SSH 镜像

宿主机准备独立构建目录：

```bash
mkdir -p /opt/101cloud/podman-gpu-ssh
cd /opt/101cloud/podman-gpu-ssh
```

`Containerfile`：

```dockerfile
FROM docker.io/library/ubuntu@sha256:78d4744ead1f5b19b04d0df558ab182a5300903900755e515b2ca4d10eb2044d
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server python3 ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /etc/ssh/ssh_host_* \
 && printf 'PermitRootLogin yes\nPasswordAuthentication yes\nPermitEmptyPasswords no\n' > /etc/ssh/sshd_config.d/00-container-root-login.conf
COPY --chmod=0755 entrypoint.sh /usr/local/sbin/container-entrypoint
EXPOSE 22
ENTRYPOINT ["/usr/local/sbin/container-entrypoint"]
```

`entrypoint.sh`：

```sh
#!/bin/sh
set -eu
mkdir -p /run/sshd
ssh-keygen -A
exec /usr/sbin/sshd -D -e
```

含义很简单：镜像安装 SSH 和测试所用的 Python；**镜像不保存 root 密码或共用 SSH 主机密钥**。每个新容器首次启动生成自己的密钥，再直接运行前台 sshd，成为 PID 1。普通重启不会重新生成已有密钥。

构建：

```bash
podman build --pull=never --network=host \
  -t localhost/ubuntu24-gpu-ssh:1 /opt/101cloud/podman-gpu-ssh
```

`--network=host` 仅用于构建时下载 Ubuntu 软件包，避免为构建另建 NAT 网络；**运行中的成员容器不使用 host 网络**。本次构建成功，没有在镜像里启动 systemd 或安装 NVIDIA 驱动。

## 5. 容器获得静态局域网 IP

先确认地址已预留/避开 DHCP，并做附加冲突检查：

```bash
arping -D -I enp14s0 -c 3 -w 5 192.168.1.152
```

本次最初候选 `.151` 收到 MAC `54:EF:44:C8:E9:C8` 的回应，已放弃；用户确认改用 `.152`，检查没有回应。不能把没有 ARP 回应当成 DHCP 预留证明。

新建 macvlan 网络，复用已有业务物理网卡，不移动宿主机 `.99`：

```bash
podman network create --driver macvlan \
  --subnet 192.168.1.0/24 --gateway 192.168.1.1 \
  --opt parent=enp14s0 --disable-dns gpu-lan
```

已有 `gpu-lan` 时先 `podman network inspect gpu-lan` 核对再复用，不删除已有网络。本网络与路由器 DHCP 并不是同一个地址管理系统，后续实例必须明确指定预留 IP，不随意自动分配。

### 固定 IP 之外，还要固定 MAC

本次第一次运行只指定 IP，Podman 重启后 MAC 发生变化，随后工作站 SSH 超时。固定 MAC、更新地址通告后恢复，并再次重启验证通过。

下面按实际验证过的网络接入流程操作，新容器初次启动后、交给用户之前固定连接信息：

```bash
podman run -d \
  --name podman-ubuntu24-gpu --hostname podman-ubuntu24-gpu \
  --systemd=false --cpus=2 --memory=4g --pids-limit=512 \
  --restart=no \
  --network gpu-lan --ip 192.168.1.152 --dns 223.6.6.6 \
  --device nvidia.com/gpu=GPU-f5f4cd05-de43-485a-10ef-8e952f3d292b \
  localhost/ubuntu24-gpu-ssh:1

podman network disconnect gpu-lan podman-ubuntu24-gpu
podman network connect --ip 192.168.1.152 \
  --mac-address b6:76:12:0a:6d:0b gpu-lan podman-ubuntu24-gpu
```

断开/连接网络只针对新测试容器，会中断其网络；不要对已有重要任务盲目执行。新增实例要选择唯一的名字、IP、MAC，不能复制本机已经使用的三个值。

本次还用宿主机工具进入该容器网络命名空间，发送了两次地址通告，以更新邻居缓存：

```bash
PID="$(podman inspect --format '{{.State.Pid}}' podman-ubuntu24-gpu)"
nsenter -t "$PID" -n arping -U -I eth0 -c 2 192.168.1.152
```

本次最终固定 MAC 为 `b6:76:12:0a:6d:0b`，再次重启仍保持不变。macvlan 默认宿主机不能直接连接该容器 IP，密码 SSH 应从局域网工作站测试。

## 6. root 密码在运行后设置，不放入镜像

在宿主机执行，交互输入需要的密码：

```bash
set +x
read -r -s -p '容器 root 密码：' ROOT_PASSWORD
printf '\n'
test -n "$ROOT_PASSWORD"
printf 'root:%s\n' "$ROOT_PASSWORD" | podman exec -i podman-ubuntu24-gpu chpasswd
unset ROOT_PASSWORD
podman exec podman-ubuntu24-gpu /usr/sbin/sshd -t
```

本次通过标准输入设置了用户指定密码，不改宿主机或 Incus 容器密码，没有把密码保存在构建文件或镜像里。弱测试密码仅用于受控局域网，不能把此 SSH 暴露到公网。

## 7. 人工验证

宿主机：

```bash
podman top podman-ubuntu24-gpu pid comm
podman exec podman-ubuntu24-gpu nvidia-smi -L
podman inspect podman-ubuntu24-gpu
```

应看到 PID 1 为 `sshd`，只列出目标 GPU UUID，网络为 `.152` 和固定 MAC，`Privileged=false`。

核对容器 SSH 主机密钥：

```bash
podman exec podman-ubuntu24-gpu ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

然后**从局域网工作站**执行，核对首次连接的指纹后输入密码：

```bash
ssh -o HostKeyAlgorithms=ssh-ed25519 \
  -o PreferredAuthentications=password -o PubkeyAuthentication=no \
  root@192.168.1.152
```

登录后可执行 `id -u` 和 `nvidia-smi -L`。不要用关闭 SSH 主机密钥检查代替核验。

### CUDA 计算验收

本次实际经 `podman exec -i -e EXPECTED_GPU_UUID=... 容器名 python3 -` 执行了现有 [CUDA 验证脚本](LXC_NVIDIA_GUIDE.md) 第 7 节的 Python 部分：只复用 Python 内容，不运行旧 PVE 的创建/配置命令。

后续复用时，把该节从 `import ctypes as C` 开始到最后 `print(...)` 为止的 Python 内容保存为宿主机 `/opt/101cloud/podman-gpu-ssh/verify_cuda.py`，不要包含外层 `pct exec`、`PY` 或 Markdown 标记。然后执行：

```bash
podman exec -i \
  -e EXPECTED_GPU_UUID=GPU-f5f4cd05-de43-485a-10ef-8e952f3d292b \
  podman-ubuntu24-gpu python3 - < /opt/101cloud/podman-gpu-ssh/verify_cuda.py
```

上述命令把保存的脚本传入容器执行，不是在等待终端手动输入。实测结果：一张可用 CUDA 卡、目标 UUID 正确、第二张设备不可用；分配 4096 字节显存并执行 `sm_61` 加 7 内核，1024 个结果全部为 130，结束后释放内存和上下文。不是只测 `nvidia-smi`，也没有安装完整 CUDA Toolkit/PyTorch。

没有重要任务后，测试普通容器重启：

```bash
podman restart podman-ubuntu24-gpu
```

本次重启后再次通过密码 SSH 和完整 CUDA 验证，密钥、IP 与固定 MAC 都保持。没有重启物理机，没有声称物理机重启后的自动恢复已经验证。

## 8. 边界和维护

- 本次 GPU 容器无 systemd，不能用 `systemctl` 管服务；主进程退出会使容器停止。
- rootful Podman 不等于 `--privileged`；本次未开 privileged，但也没有声明配置了 rootless 或与 Incus 相同的 UID 映射。
- 驱动仍为已有 `580.178.04`。CDI 自动接入运行库，不代表框架一定支持 Pascal、显存已切片或卡时计费已经实现。
- 本次没有创建用户数据卷。普通重启保留容器可写层；**删除容器会丢失该层数据**。正式工作空间应另配持久卷与备份，不能把镜像当数据备份。
- 当前 overlay 存储未验证磁盘硬配额，不把磁盘额度字段当成限制已生效。
- 现有 Incus `.150` 未改动。本次未移动网口、修改全局 DNS、升级驱动/内核或配置管理网。
- 新增文件包括 Podman 独立镜像源片段、`/opt/101cloud/podman-gpu-ssh/` 构建文件；Podman、NVIDIA 包会安装自身的服务/默认配置。
- root 密码只在当前容器可写层生效。新建另一个容器仍需单独设置，不使用镜像共用密码。
