# Ubuntu 裸机上 Incus Ubuntu 24.04 单卡容器实测

先看下面的三步简明教程；需要排错、签名核验细节或完整 CUDA 测试时，再看后面的实测记录和第 8 节。**下文 `.150` / `ubuntu24-gpu` 是当时的实测配置，不是当前资源清单；创建前必须检查现有实例与 IP。本文验证的是 Ubuntu 24.04 容器，不能只把镜像改为 Ubuntu 26.04 就假定其余条件仍成立。**

## 简明部署：实际就三步

### 第一步：宿主机装好必要组件

前提：Ubuntu 宿主机已经安装 NVIDIA 驱动，`nvidia-smi` 能正常列出显卡。

仅运行容器时，本次环境使用 `incus-base`，不需要拉入 VM 组件。除此之外，安装 NVIDIA 容器运行库和工具：

```bash
apt install incus-base
apt install libnvidia-container1 libnvidia-container-tools
```

后两个包来自 NVIDIA 官方软件源。当前机器已经安装完成，不需要重复部署。无需安装 Docker 或完整 CUDA Toolkit。

#### 人工配置 NVIDIA 官方软件源

以下命令在 **Ubuntu 宿主机 root 终端**执行，不是在容器里执行。需要已有 `curl`、`gpg` 和 CA 证书。当前实测节点为 amd64。

1. 创建公钥目录，下载官方公钥：

```bash
install -d -m 0755 /etc/apt/keyrings
NVIDIA_KEY_TMP="$(mktemp)"
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
  -o "$NVIDIA_KEY_TMP"
gpg --show-keys --with-fingerprint "$NVIDIA_KEY_TMP"
```

2. 核对主指纹。本次官方公钥为：

```text
C95B 321B 61E8 8C18 09C4 F759 DDCA E044 F796 ECB0
```

不一致就停止并核实官方密钥变更，不直接信任。确认后保存公钥；若下面的目标文件已经存在，先核对其内容，不盲目覆盖：

```bash
(set -o noclobber; cat "$NVIDIA_KEY_TMP" \
  > /etc/apt/keyrings/nvidia-container-toolkit.asc) && \
  chmod 0644 /etc/apt/keyrings/nvidia-container-toolkit.asc
```

3. 添加独立的软件源文件，并限定使用这个公钥：

```bash
(set -o noclobber; cat > /etc/apt/sources.list.d/nvidia-container-toolkit.list <<'EOF'
deb [arch=amd64 signed-by=/etc/apt/keyrings/nvidia-container-toolkit.asc] https://nvidia.github.io/libnvidia-container/stable/deb/amd64 /
EOF
)
```

公钥和源文件在当前机器上都已经存在，跳过上述新建步骤即可；不同内容的处理和更严格的核验流程见第 8.3 节。

4. 更新包清单，先检查安装计划，再安装：

```bash
apt update
apt-cache policy libnvidia-container1 libnvidia-container-tools
apt-get -s install --no-install-recommends \
  libnvidia-container1 libnvidia-container-tools
```

确认没有不明升级或删除，再执行：

```bash
apt install --no-install-recommends \
  libnvidia-container1 libnvidia-container-tools
rm "$NVIDIA_KEY_TMP"
```

这里只配置容器工具源，不安装 `nvidia-driver` 或 DKMS，不升级宿主机内核。上面是人工安装方法，不代表将来任何候选版本都已经验证；本次通过验证的版本见实测表，第 8 节也提供显式版本安装方式。

### 第二步：用配置文件创建容器

以下仍在宿主机执行。存储池不存在时只创建一次；已有池不要删除或重复创建：

```bash
incus storage create gpu-test-pool dir
```

这是目录池，不承诺磁盘硬额度。创建 `ubuntu24-gpu.yaml`，内容如下。**新实例必须修改名称、静态 IP；新节点还要核对网口名和显卡 PCI 地址。IP 要避开 DHCP 地址池。**

```yaml
config:
  boot.autostart: "false"
  security.privileged: "false"
  limits.cpu: "2"
  limits.memory: 4GiB
  nvidia.runtime: "true"
  nvidia.driver.capabilities: compute,utility
  cloud-init.network-config: |
    version: 2
    ethernets:
      eth0:
        dhcp4: false
        dhcp6: false
        accept-ra: false
        addresses:
          - 192.168.1.150/24
        routes:
          - to: default
            via: 192.168.1.1
        nameservers:
          addresses:
            - 223.6.6.6
devices:
  root:
    type: disk
    path: /
    pool: gpu-test-pool
  eth0:
    type: nic
    nictype: macvlan
    parent: enp14s0
    name: eth0
  gpu:
    type: gpu
    gputype: physical
    pci: "0000:05:00.0"
```

创建并启动：

```bash
incus launch images:ubuntu/24.04/cloud ubuntu24-gpu \
  --no-profiles < ubuntu24-gpu.yaml
```

`gpu` 指定显卡，`nvidia.runtime=true` 自动注入运行库；不需要复制驱动文件。静态地址不是由 macvlan 自动发放：它创建网卡后，仍由**容器内的 cloud-init、netplan 和网络服务**应用 `cloud-init.network-config`。所以必须使用带 cloud-init 的镜像，并在首次启动前传入配置；已经启动过的实例不能假定修改这一配置后就会自动重新初始化网络。启动返回不等于系统就绪，等待命令见第 8.6 节；服务长期卡住时要查错误，不能无限等待。

当前已有实例只需启动，不要再次 launch：

```bash
incus start ubuntu24-gpu
```

已经运行时这一步也无需执行。macvlan 默认不能从宿主机直接连接容器 IP，局域网工作站可以访问，宿主机使用 `incus exec` 管理。

#### Ubuntu 26.04 有网卡但没有 IPv4：本环境已发现的兼容问题

2026-10-28 对用户新建的 `ubuntu26-gpu` 只读诊断发现：镜像为 `ubuntu/26.04/cloud`，macvlan 的父接口和 `eth0` 已存在且为 UP，Incus 也保存了 `192.168.1.11/24` 的正确静态配置。但 `systemd-networkd` 一直停在 activating，出现多组 `sd-mkuserns` 子进程，cloud-init 一直 running；宿主机 AppArmor 日志拒绝该容器相关进程的 signal/ptrace 操作。

宿主机当时 `kernel.apparmor_restrict_unprivileged_unconfined=1`。与 [Incus 上游同类问题 #3251](https://github.com/lxc/incus/issues/3251) 相符：Ubuntu 特有的额外 AppArmor 用户命名空间限制可能阻塞 Ubuntu 26 / systemd 259 初始化。**这类无 IP 不是 DHCP 故障，也不应通过随机更换网关、地址或关闭全部 AppArmor 来处理。** Ubuntu 24 的原有成功记录不代表 Ubuntu 26 已兼容。

诊断命令（宿主机执行，实例名按实际替换）：

```bash
incus config show ubuntu26-gpu --expanded
incus exec ubuntu26-gpu -- cloud-init status --long
incus exec ubuntu26-gpu -- systemctl status systemd-networkd --no-pager
sysctl kernel.apparmor_restrict_unprivileged_unconfined
journalctl -k --since '10 minutes ago' --no-pager --grep='apparmor=.*DENIED'
```

上游建议可将 `kernel.apparmor_restrict_unprivileged_unconfined` 设为 0，恢复标准 AppArmor 行为。这**不是关闭 AppArmor**，但会取消 Ubuntu 的一项宿主机范围额外限制，不能由容器教程静默更改。需要管理员明确授权后才可临时测试，并仅重启受影响容器、核验 IP；验证成功后再决定是否持久化。另一条不改此开关的路线是继续使用本文已验证的 Ubuntu 24.04 镜像，但不应未经确认删除用户已有容器来重建。

本次获得用户明确授权后，先检查 `.11` 未收到 ARP 冲突回应，再执行了以下临时修复：

```bash
sysctl -w kernel.apparmor_restrict_unprivileged_unconfined=0
incus restart ubuntu26-gpu --timeout 30
```

修复后 systemd 为 `running`、cloud-init 为 `done`、失败服务为 0，`eth0` 已配置 `192.168.1.11/24`，默认网关为 `192.168.1.1`，指定 GPU UUID 查询正常；工作站 ping 两次全部收到回应。Podman 容器继续运行，未重启。**只修改了运行时参数，未写 `/etc/sysctl.d` 等持久配置；宿主机重启后可能恢复原值并再次遇到问题。** 是否长期启用需要单独决定，不把这次临时验证当成持久修复。


### 第三步：容器内只配 SSH 和自己的应用

systemd 由 Ubuntu 24.04 系统镜像提供；GPU 设备、运行库和静态 IP 已在创建配置中处理。**不在容器里安装 NVIDIA 内核驱动。**

若要 root 密码 SSH 登录，先在宿主机进入容器：

```bash
incus exec ubuntu24-gpu -- bash
```

下面改为在**容器内**执行：

```bash
apt update
apt install -y openssh-server
passwd root
```

密码在提示中输入，不保存到文档或脚本。为新容器创建 `/etc/ssh/sshd_config.d/00-studio-root-login.conf`，内容为：

```text
PermitRootLogin yes
PasswordAuthentication yes
PermitEmptyPasswords no
```

然后：

```bash
systemctl enable --now ssh
sshd -t
systemctl restart ssh
nvidia-smi -L
```

从局域网工作站执行 `ssh root@192.168.1.150`，核对主机密钥后用刚设置的密码登录。不要把弱测试密码的 SSH 映射到公网。PyTorch 等应用另行选择支持 Pascal `sm_61` 的版本；看到显卡不等于任意框架版本都能运行。

**一句话：宿主机装驱动、Incus 和 NVIDIA 容器库；创建时声明显卡和网络；容器内只配置 SSH 与应用。当前实例的这些步骤均已完成。**

## 1. 最终结果

在用户已重装为 Ubuntu 的 `192.168.1.99` 上完成了 Incus 系统容器验证。原先的 PVE/CT 101 记录属于旧环境，不能把旧教程直接用在当前主机上。

| 项目 | 实测/配置结果 |
| --- | --- |
| 宿主机 | `101cloud-0`，Ubuntu 26.04.1 LTS |
| 宿主内核 | `7.0.0-38-generic` |
| NVIDIA 驱动 | 已存在的 `580.178.04`，本次没有升级 |
| Incus | Ubuntu 仓库 `incus-base` / `incus-client`，`6.0.5-8`，客户端与服务端 `6.0.5` |
| LXC 库 | `liblxc1t64` / `liblxc-common`，`1:6.0.6-1ubuntu1` |
| NVIDIA 容器库 | `libnvidia-container1` / `libnvidia-container-tools`，`1.20.1-1` |
| 容器名称 | `ubuntu24-gpu` |
| 容器系统 | Ubuntu 24.04 noble，系统容器，cloud 镜像 |
| 镜像标识 | `images:ubuntu/24.04/cloud`；指纹 `967b69f786b229afbfaabb40aa8c86027ce9a2e31dc596d367231f272ef268ff` |
| systemd | PID 1 为 `/sbin/init`；运行状态 `running`；失败服务为 0 |
| 隔离 | `security.privileged=false`；没有开启 nesting 或设置 AppArmor unconfined |
| 资源 | CPU 限制 2，内存限制 4 GiB |
| 静态地址 | **`192.168.1.150/24`**，用户确认可用并已避开 DHCP |
| 网关 / DNS | `192.168.1.1` / `223.6.6.6` |
| 网卡 | macvlan，父接口为现有 `enp14s0` |
| GPU | 一张 NVIDIA TITAN X (Pascal)，UUID `GPU-7f665bf9-5911-4f71-ac6a-a66e32362574` |
| GPU 选择 | Incus `gpu` 设备，PCI `0000:05:00.0` |
| NVIDIA 接入 | `nvidia.runtime=true`，能力 `compute,utility` |
| SSH | 容器内 OpenSSH，root 密码认证已开启，密码按用户要求设置；本文不保存明文密码 |
| 存储 | 新建目录型池 `gpu-test-pool`；**没有承诺或配置磁盘硬容量配额** |
| 自启 | `boot.autostart=false`；当前容器保持运行 |

用户可从同一局域网工作站连接：

```bash
ssh root@192.168.1.150
```

宿主机管理员也可以使用：

```bash
incus exec ubuntu24-gpu -- bash
```

## 2. 本次宿主机变更范围

安装了容器所需的 Incus 组件、NVIDIA 容器库/工具，以及静态地址冲突检查工具 `iputils-arping`。

使用 `incus-base` 而不是 `incus` 元包，是因为当前 Ubuntu 仓库的 `incus` 元包会依赖 QEMU 等 VM 组件；本次只需要容器。安装计划与执行结果没有升级现有软件包，没有安装 VM 运行组件，也没有执行 autoremove。

NVIDIA 容器库来自独立的官方签名仓库：

- 公钥：`/etc/apt/keyrings/nvidia-container-toolkit.asc`。
- 源配置：`/etc/apt/sources.list.d/nvidia-container-toolkit.list`，使用限定 `signed-by`，没有修改原有 Ubuntu 源。
- 公钥主指纹核对为 `C95B321B61E88C1809C4F759DDCAE044F796ECB0`。

没有重装或升级 NVIDIA 驱动，没有重启宿主机，没有移动 `192.168.1.99` 的网口地址，没有创建宿主机 Linux bridge/NAT 网络，没有修改路由器 DHCP。没有开放 Incus 的网络 API；`core.https_address` 为空，管理走本地 socket/现有 SSH。

创建的 Incus 资源只有新目录存储池及目标容器。没有运行默认网络自动初始化，没有另外创建 GPU 测试实例。

## 3. 单卡与自动运行库注入

关键配置：

```yaml
config:
  boot.autostart: "false"
  limits.cpu: "2"
  limits.memory: 4GiB
  security.privileged: "false"
  nvidia.runtime: "true"
  nvidia.driver.capabilities: compute,utility
devices:
  root:
    type: disk
    path: /
    pool: gpu-test-pool
  eth0:
    type: nic
    nictype: macvlan
    parent: enp14s0
    name: eth0
  gpu:
    type: gpu
    gputype: physical
    pci: "0000:05:00.0"
```

这是 Incus 原生 NVIDIA 接入，不是本次手写的 CDI 适配。宿主机提供内核驱动，Incus 配合 `libnvidia-container` 接入本节点的 NVIDIA 用户态文件。

**没有像旧 PVE 试验那样，手工往容器复制 NVIDIA 库。** 对运行态检查 `findmnt`，确认以下文件从宿主机只读挂载进入容器：

- `libcuda.so.580.178.04`：`ro,nosuid,nodev,relatime`。
- `libnvidia-ml.so.580.178.04`：`ro,nosuid,nodev,relatime`。

宿主机的 GPU 数字序号、Incus 资源列表的 Card 序号、容器内序号仍可能不同。平台应保存 GPU UUID，操作前从本节点资源中解析对应 PCI 地址，启动后再核对 UUID。不要把容器内的 `GPU 0` 当成宿主机的第 0 张卡，也不要省略 GPU 选择条件而暴露所有卡。

以后升级节点驱动仍需维护窗口和重新验证；自动注入不等于运行期间可以无风险热升级。

## 4. 静态局域网 IP 的实现

没有把宿主机网口改造成网桥，而是让新容器使用 `enp14s0` 上的 macvlan。

用户确认 `192.168.1.150` 已预留/避开 DHCP 后，额外执行了针对该地址的 ARP 重复地址检查：发送 3 个探测，收到 0 个回应。ARP 检查只是附加验证，不替代用户的 DHCP 预留确认。

创建前通过 `cloud-init.network-config` 注入：

```yaml
version: 2
ethernets:
  eth0:
    dhcp4: false
    dhcp6: false
    accept-ra: false
    addresses:
      - 192.168.1.150/24
    routes:
      - to: default
        via: 192.168.1.1
    nameservers:
      addresses:
        - 223.6.6.6
```

容器重启后仍为 `192.168.1.150/24`，不是 DHCP 临时地址。观察到的 IPv6 只有链路本地地址，没有为其配置全局 IPv6。

**macvlan 默认不能让宿主机通过父接口直接访问容器。** 本次实际密码 SSH 测试从管理员工作站发起，而不是从 `192.168.1.99` 发起。宿主机通过 `incus exec` 管理不受这个限制。若后续要求宿主机直接访问容器业务服务，需要单独设计宿主机侧 macvlan 接口或改网络方式，不要把这个默认限制误判成容器 IP 配置失败。

## 5. root 密码 SSH 登录

Ubuntu 24.04 cloud 镜像生成了 SSH 主机密钥，但本次镜像最初没有安装 OpenSSH 服务端。只在目标容器里更新 apt 元数据并安装了 `openssh-server` 及其依赖，没有升级整个系统。

只修改容器，不修改宿主机 root 密码或宿主机 SSH 配置：

- 使用 `incus exec ... -- chpasswd` 经标准输入设置用户要求的密码，不把密码放入远程命令参数。
- 通过 Incus 文件接口创建容器内 `/etc/ssh/sshd_config.d/00-studio-root-login.conf`，root 所有、权限 `0600`。
- 配置 `PermitRootLogin yes`、`PasswordAuthentication yes`、`PermitEmptyPasswords no`。
- 启用并启动容器内 `ssh.service`，执行 `sshd -t`，回读 `sshd -T` 确认生效。

本次观察到 Ubuntu 的 `ssh.socket` 已监听，但服务最初仍为 inactive；因此不能只看 socket 或容器运行状态就声称 SSH 服务已经就绪。服务启动后配置语法检查与状态均正常。

使用系统自带的交互 SSH 验证工具，从管理员工作站实际执行了**仅密码认证**的 root 登录，禁用客户端公钥认证；并用通过已认证宿主机/Incus 读取的容器公钥核验 SSH 主机身份。没有关闭主机密钥验证，没有向工作站永久写入测试密钥文件。

登录后确认 UID 为 0、systemd 为 running、GPU UUID 正确；容器重启后再次通过同一测试，容器 SSH 主机密钥保持不变。

用户选择的是弱测试密码。只应在受控局域网短期使用，不应把该 SSH 服务直接映射到公网；完成测试后应改强密码或公钥认证。

## 6. CUDA 与重启验证结果

通过容器内 Python 调用 CUDA Driver API，不安装 PyTorch、编译器或完整 CUDA Toolkit：

- 只发现 1 张可访问 CUDA GPU。
- UUID 为 `GPU-7f665bf9-5911-4f71-ac6a-a66e32362574`。
- 获取第二张 CUDA 设备返回 invalid device。
- 分配 4096 字节显存，初始化 1024 个整数为 123。
- 加载 `sm_61` PTX 并在 GPU 上执行加 7 内核。
- 同步、读回，1024 个结果全部为 130。
- 结束时释放显存、模块与上下文，测试通过标准输入执行，没有在容器里留下运行中的验证程序。

只重启了新容器一次，重启后重新确认：

| 检查项 | 结果 |
| --- | --- |
| systemd | `running`，失败 unit 数 0 |
| cloud-init | `done` |
| 静态 IP | 保持 `192.168.1.150/24` |
| SSH 服务 | `active` |
| root 密码登录 | 从工作站再次成功 |
| SSH 主机密钥 | 与重启前一致 |
| 单卡身份 | 目标 UUID 保持不变 |
| 真实 CUDA 内核计算 | 再次通过，1024 个结果全部正确 |

未重启物理机，未验证驱动热升级、两个容器共享压力、运行中 GPU 换卡或任意应用框架兼容性。

## 7. 后续开发需要记住的事情

1. **容器启动 API 返回，不等于系统就绪。** 本次刚 `incus start` 后立即访问 systemd 总线曾得到 socket 尚不存在；应等待 guest 初始化，再分别验证 systemd、cloud-init、网络和 SSH，不要先改安全设置。
2. **这次 Ubuntu 24.04 无需 nesting 兼容补丁。** 不应照搬旧 PVE / Ubuntu 26 的参数组合。
3. **仅容器场景装 `incus-base`。** 当前仓库 `incus` 元包会拉入 QEMU 等 VM 依赖。
4. **当前是 Incus 6.0.5 系统容器试验。** 不能据此声称此前讨论的 OCI 应用容器、CDI 路径也已验证；后续有这些需求时要另行选择支持的版本并测试。
5. **显卡驱动库跟节点走。** 当前原生只读注入已经工作，但仍需锁定节点兼容矩阵和版本升级流程。
6. **单卡不是显存分片。** 此试验没有实现每用户卡数策略、显存硬限额或卡时计费。
7. **目录池不是磁盘硬额度。** 这次为最小验证使用 `dir` 池，没有把一个根盘 size 字段当成已生效的硬配额；生产方案需要另选并验证合适的存储与配额方式。
8. **静态 IP 需要真实地址管理。** 不用扫描结果代替 DHCP 排除/预留，也不把 macvlan 的宿主机隔离当成认证或完整安全边界。
9. **密码不进镜像或仓库。** 平台后续通过受控流程设置或轮换用户凭据，不把本次测试密码写成全体实例默认密码。

## 8. 部署步骤：怎么做

下面把本次操作整理成可复用步骤。适用于已经装好驱动的 **Ubuntu 26.04 x86_64 宿主机**，创建 Ubuntu 24.04 系统容器。不是一键安装脚本，也没有在编写文档时重新连接或修改服务器。

### 8.1 连接宿主机，确认地址和显卡

先在工作站连接宿主机：

```bash
ssh root@192.168.1.99
```

除特别标注的工作站验证外，以下命令都在**同一个宿主机 Bash 会话**中执行，不要换终端后直接使用之前的变量。

```bash
set -euo pipefail
hostnamectl
uname -r
nvidia-smi -L
ip -br address
ip route show
resolvectl dns enp14s0
command -v curl
command -v gpg
command -v python3
```

宿主机自己都不能运行 `nvidia-smi` 时，先停止这份流程，单独处理驱动；不要在容器里安装驱动来补救。缺少上述基础工具时也先补齐并核对影响，不执行整机升级。

本次使用的参数如下。**在当前主机上 `ubuntu24-gpu` 和 `192.168.1.150` 已经占用。新增实例必须换名字和已预留的 IP；新节点还要改物理网口及 GPU UUID。**

```bash
export INSTANCE=ubuntu24-gpu
export POOL=gpu-test-pool
export PARENT=enp14s0
export STATIC_IP=192.168.1.150
export PREFIX=24
export GATEWAY=192.168.1.1
export DNS=223.6.6.6
export GPU_UUID=GPU-7f665bf9-5911-4f71-ac6a-a66e32362574
export IMAGE=images:ubuntu/24.04/cloud
```

IP 必须经过 DHCP 排除或预留确认。下面会做 ARP 检查，但 ARP 没有回应不等于地址永远没人用。

### 8.2 只安装容器版 Incus

```bash
apt-cache policy incus-base
apt-get -s install --no-install-recommends incus-base=6.0.5-8
```

先检查模拟结果，没有驱动/内核升级、删除或其他不明变化，再执行：

```bash
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  --no-install-recommends incus-base=6.0.5-8

incus version
incus list
incus storage list
```

版本号是本次实测值。新节点仓库没有这个版本时先重新确认兼容性，不要为了跑通直接添加多个 Incus 源或升级整机。`incus-base` 已包含本次系统容器所需组件，不使用会拉入 QEMU 的 `incus` 元包。

这里**不执行 `incus admin init --auto`**，避免自动创建不需要的 NAT 网桥；后面显式创建目录池和容器网卡。

### 8.3 安装 NVIDIA 容器库，不动内核驱动

先下载 NVIDIA 官方仓库公钥并核对主指纹，再写独立源文件。临时目录只存公钥和源描述，不存密码。

```bash
NVIDIA_TMP="$(mktemp -d)"
trap 'rm -f "$NVIDIA_TMP/key.asc" "$NVIDIA_TMP/source.list"; rmdir "$NVIDIA_TMP"' EXIT

curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
  -o "$NVIDIA_TMP/key.asc"

NVIDIA_FPR="$(gpg --batch --with-colons --show-keys "$NVIDIA_TMP/key.asc" \
  | python3 -c 'import sys; print(next(line.split(":")[9] for line in sys.stdin if line.startswith("fpr:")))')"
test "$NVIDIA_FPR" = C95B321B61E88C1809C4F759DDCAE044F796ECB0

printf '%s\n' \
  'deb [arch=amd64 signed-by=/etc/apt/keyrings/nvidia-container-toolkit.asc] https://nvidia.github.io/libnvidia-container/stable/deb/amd64 /' \
  > "$NVIDIA_TMP/source.list"

if [ -e /etc/apt/keyrings/nvidia-container-toolkit.asc ]; then
  cmp "$NVIDIA_TMP/key.asc" /etc/apt/keyrings/nvidia-container-toolkit.asc
else
  install -D -m 0644 "$NVIDIA_TMP/key.asc" \
    /etc/apt/keyrings/nvidia-container-toolkit.asc
fi

if [ -e /etc/apt/sources.list.d/nvidia-container-toolkit.list ]; then
  cmp "$NVIDIA_TMP/source.list" /etc/apt/sources.list.d/nvidia-container-toolkit.list
else
  install -m 0644 "$NVIDIA_TMP/source.list" \
    /etc/apt/sources.list.d/nvidia-container-toolkit.list
fi

apt-get update
apt-get -s install --no-install-recommends \
  libnvidia-container1=1.20.1-1 \
  libnvidia-container-tools=1.20.1-1 iputils-arping
```

现有同名文件与预期不同，`cmp` 会阻止继续，不盲目覆盖。主指纹变了、源不可用或版本不存在，也应先核实原因。

模拟结果符合预期后：

```bash
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  --no-install-recommends \
  libnvidia-container1=1.20.1-1 \
  libnvidia-container-tools=1.20.1-1 iputils-arping
```

这一步安装的是 Incus 自动注入要用的库和工具。**不安装 `nvidia-driver`、DKMS、Docker 或完整 CUDA Toolkit，不重新安装宿主机驱动。**

### 8.4 检查重名、地址和 GPU 对应关系

先阻止覆盖已有容器：

```bash
incus list --format=json | python3 -c \
  'import json,os,sys; name=os.environ["INSTANCE"]; rows=json.load(sys.stdin); assert not any(x["name"]==name for x in rows), "同名容器已存在，停止；不要删除它来重跑教程"'

arping -D -I "$PARENT" -c 3 -w 5 "$STATIC_IP"
nvidia-smi -i "$GPU_UUID" --query-gpu=uuid,pci.bus_id,driver_version --format=csv
```

ARP 收到其他设备回应时停止，换一个真正可用的地址，不把对方挤掉。创建前还要检查已有 Incus 实例没有使用同一个地址。

本次显卡序号与设备次编号不同，所以按 UUID 查询 PCI 地址，再交给 Incus 选设备：

```bash
GPU_PCI="$(nvidia-smi -i "$GPU_UUID" --query-gpu=pci.bus_id \
  --format=csv,noheader,nounits \
  | python3 -c 'import sys; domain,bus,slot=sys.stdin.read().strip().split(":"); print(f"{int(domain,16):04x}:{bus.lower()}:{slot.lower()}")')"
export GPU_PCI
printf 'GPU UUID=%s\nPCI=%s\n' "$GPU_UUID" "$GPU_PCI"
incus image info "$IMAGE"
```

本机目标输出应为 `0000:05:00.0`。新节点以实际查询结果为准，不手工把 GPU 序号拼成 `/dev/nvidiaN`。镜像别名会更新，本次镜像指纹见第 1 节；后续应记录新实例实际使用的指纹。

### 8.5 准备存储并创建容器

本次新建的是目录型存储池：

```bash
incus storage list
incus storage create "$POOL" dir
```

**只有池不存在时才执行创建命令。** 若要复用已有池，先确认其用途与配置，再跳过创建；不要删除已有池来凑空环境。目录池不格式化现有磁盘，但也不在本方案里提供磁盘硬容量配额。

将创建配置直接传给 Incus，不额外写宿主机网口配置：

```bash
incus init "$IMAGE" "$INSTANCE" --no-profiles <<EOF
config:
  boot.autostart: "false"
  limits.cpu: "2"
  limits.memory: 4GiB
  security.privileged: "false"
  nvidia.runtime: "true"
  nvidia.driver.capabilities: compute,utility
  cloud-init.network-config: |
    version: 2
    ethernets:
      eth0:
        dhcp4: false
        dhcp6: false
        accept-ra: false
        addresses:
          - ${STATIC_IP}/${PREFIX}
        routes:
          - to: default
            via: ${GATEWAY}
        nameservers:
          addresses:
            - ${DNS}
devices:
  root:
    type: disk
    path: /
    pool: ${POOL}
  eth0:
    type: nic
    nictype: macvlan
    parent: ${PARENT}
    name: eth0
  gpu:
    type: gpu
    gputype: physical
    pci: "${GPU_PCI}"
EOF
```

`--no-profiles` 避免继承未知默认网卡、磁盘或 GPU。`gpu` 指定一张卡，`nvidia.runtime=true` 自动注入运行库；不需要再复制旧 PVE 教程里的 NVIDIA 库，也不配置 CDI 或开放全部卡。

### 8.6 启动后等 systemd 和 cloud-init 就绪

```bash
incus start "$INSTANCE"

incus exec "$INSTANCE" -- python3 - <<'PY'
import os
import time
deadline = time.monotonic() + 45
while not os.path.exists('/run/systemd/private'):
    if time.monotonic() >= deadline:
        raise SystemExit('systemd 控制 socket 未就绪，停止并检查启动日志')
    time.sleep(0.2)
print('systemd control socket ready')
PY

timeout 90s incus exec "$INSTANCE" -- systemctl is-system-running --wait
timeout 120s incus exec "$INSTANCE" -- cloud-init status --wait
incus exec "$INSTANCE" -- systemctl --failed --no-pager
incus exec "$INSTANCE" -- ip -br address
incus exec "$INSTANCE" -- nvidia-smi -L
```

预期：systemd 为 `running`，cloud-init 为 `done`，失败服务为 0，`eth0` 是指定静态地址，显卡 UUID 与选定卡一致。失败时先查具体原因，不默认加 nesting、改特权模式或关闭 AppArmor。不要在宿主机上用 SSH 直连 macvlan 容器来判断网络是否配置成功。

### 8.7 只在容器里安装 SSH、设置 root 密码

先安装服务端：

```bash
incus exec "$INSTANCE" -- apt-get update
incus exec "$INSTANCE" -- env DEBIAN_FRONTEND=noninteractive \
  apt-get install -y --no-install-recommends openssh-server
```

为这个新容器写独立覆盖文件，不修改宿主机的 `/etc/ssh`：

```bash
incus exec "$INSTANCE" -- test ! -e /etc/ssh/sshd_config.d/00-studio-root-login.conf
incus file push --mode 0600 --uid 0 --gid 0 - \
  "$INSTANCE/etc/ssh/sshd_config.d/00-studio-root-login.conf" <<'SSHD'
# Container-only root password login requested by the owner.
PermitRootLogin yes
PasswordAuthentication yes
PermitEmptyPasswords no
SSHD

set +x
read -r -s -p '设置容器 root 密码（不会显示）：' CONTAINER_ROOT_PASSWORD
printf '\n'
test -n "$CONTAINER_ROOT_PASSWORD"
printf 'root:%s\n' "$CONTAINER_ROOT_PASSWORD" | incus exec "$INSTANCE" -- chpasswd
unset CONTAINER_ROOT_PASSWORD

incus exec "$INSTANCE" -- systemctl enable --now ssh.service
incus exec "$INSTANCE" -- /usr/sbin/sshd -t
incus exec "$INSTANCE" -- /usr/sbin/sshd -T
incus exec "$INSTANCE" -- systemctl is-active ssh.service
incus exec "$INSTANCE" -- passwd -S root
```

密码在交互提示里输入，不放进命令行参数、镜像或文档。`sshd -T` 中确认 `permitrootlogin yes`、`passwordauthentication yes`、`permitemptypasswords no`；服务为 `active`，root 不再是锁定密码状态。这里没有改变 SSH 监听端口，默认 22。

### 8.8 从局域网工作站验证密码登录

先在宿主机获取容器公钥指纹，准备与首次 SSH 提示核对：

```bash
incus exec "$INSTANCE" -- ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

**接下来在工作站的另一个终端执行，不是在宿主机里执行。** IP 改成自己选择的静态地址：

```bash
ssh -o HostKeyAlgorithms=ssh-ed25519 \
  -o PreferredAuthentications=password \
  -o PubkeyAuthentication=no \
  -o KbdInteractiveAuthentication=no \
  root@192.168.1.150
```

核对主机密钥指纹，输入刚设置的密码。不要用 `StrictHostKeyChecking=no` 跳过身份校验。登录后执行：

```bash
id -u
systemctl is-system-running
ip -br address
nvidia-smi -L
```

预期 UID 为 0、systemd 正常、地址及 GPU UUID 正确。能查卡不等于已验证 CUDA 计算，下一步继续测。

### 8.9 验证真实 CUDA 内核计算

回到前面保留变量的宿主机终端执行。下面与本次实测相同，通过 Python 的 CUDA Driver API 执行 `sm_61` 内核，不安装 Toolkit、编译器或 PyTorch。

```bash
incus exec "$INSTANCE" -- env EXPECTED_GPU_UUID="$GPU_UUID" python3 - <<'PY'
import ctypes as C
import json
import os
import uuid

cuda = C.CDLL('libcuda.so.1')
P, U, I, D = C.c_void_p, C.c_uint, C.c_int, C.c_uint64
signatures = {
    'cuInit': [U],
    'cuDeviceGetCount': [C.POINTER(I)],
    'cuDeviceGet': [C.POINTER(I), I],
    'cuDeviceGetUuid': [P, I],
    'cuCtxCreate_v2': [C.POINTER(P), U, I],
    'cuCtxDestroy_v2': [P],
    'cuMemAlloc_v2': [C.POINTER(D), C.c_size_t],
    'cuMemFree_v2': [D],
    'cuMemsetD32_v2': [D, U, C.c_size_t],
    'cuMemcpyDtoH_v2': [P, D, C.c_size_t],
    'cuModuleLoadData': [C.POINTER(P), P],
    'cuModuleGetFunction': [C.POINTER(P), P, C.c_char_p],
    'cuModuleUnload': [P],
    'cuLaunchKernel': [P] + [U] * 7 + [P, C.POINTER(P), C.POINTER(P)],
    'cuCtxSynchronize': [],
    'cuGetErrorName': [I, C.POINTER(C.c_char_p)],
}
for symbol, types in signatures.items():
    getattr(cuda, symbol).argtypes = types
    getattr(cuda, symbol).restype = I

def call(symbol, *args):
    code = getattr(cuda, symbol)(*args)
    if code:
        label = C.c_char_p()
        cuda.cuGetErrorName(code, C.byref(label))
        raise RuntimeError(f'{symbol}: {code} {label.value!r}')

call('cuInit', 0)
count, device = I(), I()
call('cuDeviceGetCount', C.byref(count))
assert count.value == 1, f'预期只有一张可用卡，实际 {count.value}'
call('cuDeviceGet', C.byref(device), 0)
uuid_bytes = (C.c_ubyte * 16)()
call('cuDeviceGetUuid', uuid_bytes, device)
actual_uuid = 'GPU-' + str(uuid.UUID(bytes=bytes(uuid_bytes)))
assert actual_uuid == os.environ['EXPECTED_GPU_UUID'], actual_uuid
assert cuda.cuDeviceGet(C.byref(I()), 1) == 101, '发现了第二张可用卡'

ptx = b'''.version 7.0
.target sm_61
.address_size 64
.visible .entry add_seven(.param .u64 output, .param .u32 count) {
    .reg .pred %p;
    .reg .b32 %r<7>;
    .reg .b64 %rd<4>;
    ld.param.u64 %rd0, [output];
    ld.param.u32 %r0, [count];
    mov.u32 %r1, %tid.x;
    mov.u32 %r2, %ctaid.x;
    mov.u32 %r3, %ntid.x;
    mad.lo.s32 %r4, %r2, %r3, %r1;
    setp.ge.u32 %p, %r4, %r0;
    @%p bra DONE;
    mul.wide.u32 %rd1, %r4, 4;
    add.s64 %rd2, %rd0, %rd1;
    ld.global.u32 %r5, [%rd2];
    add.u32 %r6, %r5, 7;
    st.global.u32 [%rd2], %r6;
DONE:
    ret;
}
'''
context, module, memory = P(), P(), D()
try:
    call('cuCtxCreate_v2', C.byref(context), 0, device)
    n = 1024
    call('cuMemAlloc_v2', C.byref(memory), n * 4)
    call('cuMemsetD32_v2', memory, 123, n)
    ptx_buffer = C.create_string_buffer(ptx)
    call('cuModuleLoadData', C.byref(module), C.cast(ptx_buffer, P))
    kernel = P()
    call('cuModuleGetFunction', C.byref(kernel), module, b'add_seven')
    length = U(n)
    params = (P * 2)(C.cast(C.byref(memory), P), C.cast(C.byref(length), P))
    call('cuLaunchKernel', kernel, 4, 1, 1, 256, 1, 1, 0, None, params, None)
    call('cuCtxSynchronize')
    output = (U * n)()
    call('cuMemcpyDtoH_v2', output, memory, n * 4)
    assert all(value == 130 for value in output), 'GPU 计算结果错误'
finally:
    if module.value:
        call('cuModuleUnload', module)
    if memory.value:
        call('cuMemFree_v2', memory)
    if context.value:
        call('cuCtxDestroy_v2', context)

print(json.dumps({
    'status': 'PASS', 'gpu_uuid': actual_uuid,
    'visible_gpu_count': count.value,
    'verified_elements': n, 'result_each': 130,
}))
PY
```

预期输出包含 `PASS`、目标 UUID、一张卡，以及 1024 个结果均为 130。这个脚本针对本次 Pascal 单卡环境，不应当作任意架构、任意多卡规格的通用验收程序。

### 8.10 只重启容器，复查持久性

这一步会中断新容器中的 SSH 和计算，确认没有重要任务后再做；**不是重启宿主机**。

```bash
incus restart "$INSTANCE"
```

然后重复第 8.6 节的就绪检查、第 8.8 节的工作站密码 SSH、第 8.9 节的 CUDA 测试。应保持相同静态 IP、SSH 主机密钥与 GPU UUID。

最后查看：

```bash
incus list
incus config show "$INSTANCE" --expanded
incus config get core.https_address
```

本次 Incus 网络 API 地址为空，不因为测试而开放 8443。整个流程不需要移动宿主机地址、不需要默认 NAT 网络、不需要复制显卡库、不需要改宿主机 root 密码，也不需要开启特权或 unconfined 模式。

新节点的驱动、内核、网卡、地址池或包版本不同，都要重新核对并验收；不因为照抄命令就宣称已经兼容。
