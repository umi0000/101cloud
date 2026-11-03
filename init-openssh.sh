#!/bin/sh
# 101cloud 客机初始化：Ubuntu / Debian / Arch Linux，需以 root 执行。
# 安装 OpenSSH，监听 IPv4 TCP 22；按 cloud-init 登录方式启用密码或仅公钥认证。
# 无平台模式标记时兼容原行为：允许 root 使用已有的非空密码登录。
# 不设置密码、不修改防火墙、不下载执行远程脚本。推荐在 cloud_init 阶段使用。
# 扩展系统时，只需增加下面的包管理器和服务映射；公共 SSH 配置保持一致。
set -eu

log() { printf '%s\n' "[init-openssh] $*" >&2; }
fail() { log "错误：$*"; exit 1; }

config=/etc/ssh/sshd_config
candidate=
backup=
activated=0
complete=0
service_name=
manager=

cleanup() {
    result=$?
    trap - 0
    if [ "$activated" -eq 1 ] && [ "$complete" -eq 0 ]; then
        log "初始化未完成，恢复 SSH 配置：$backup"
        if cp -p "$backup" "$config"; then
            if [ "$manager" = systemd ]; then
                systemctl restart "$service_name" || log "警告：恢复配置后服务仍无法启动，请检查系统日志。"
            elif [ "$manager" = sysv ]; then
                service "$service_name" restart || log "警告：恢复配置后服务仍无法启动，请检查系统日志。"
            fi
        else
            log "警告：恢复配置失败，请使用备份手工恢复。"
        fi
    fi
    if [ -n "$candidate" ]; then rm -f "$candidate"; fi
    exit "$result"
}
trap cleanup 0
trap 'exit 130' INT
trap 'exit 143' TERM

[ "$(id -u)" -eq 0 ] || fail '必须在目标实例内以 root 执行。'
# cloud-init 只写固定模式值；不能将该文件作为 Shell 脚本加载。
login_mode=password
mode_file=/etc/101cloud/ssh-login-mode
if [ -e "$mode_file" ]; then
    [ -f "$mode_file" ] && [ ! -L "$mode_file" ] && [ -r "$mode_file" ] || fail 'SSH 登录模式标记必须是可读的普通文件。'
    login_mode=$(cat "$mode_file")
fi
case "$login_mode" in
    password) root_policy=yes; password_auth=yes ;;
    ssh-key) root_policy=prohibit-password; password_auth=no ;;
    *) fail 'SSH 登录模式标记无效，拒绝修改配置。' ;;
esac
[ -r /etc/os-release ] || fail '缺少 /etc/os-release，无法识别系统。'
# os-release 是由操作系统维护的本地身份文件，不执行用户输入。
. /etc/os-release
case "${ID:-}" in
    ubuntu|debian)
        family=debian
        service_name=ssh
        ;;
    arch|archlinux)
        family=arch
        service_name=sshd
        ;;
    *) fail "暂不支持系统 ${ID:-unknown}；请先增加并验证系统适配。" ;;
esac

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    manager=systemd
elif [ "$family" = debian ] && command -v service >/dev/null 2>&1 && command -v update-rc.d >/dev/null 2>&1; then
    manager=sysv
else
    fail '未找到受支持的服务管理器；需要 systemd，或 Debian/Ubuntu 的 SysV 服务管理器。'
fi

case "$family" in
    debian)
        command -v apt-get >/dev/null 2>&1 || fail '缺少 apt-get。'
        log '安装 openssh-server。'
        apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server
        ;;
    arch)
        command -v pacman >/dev/null 2>&1 || fail '缺少 pacman。'
        log '安装 openssh；使用现有同步数据库，不执行单独 -Sy 或全系统升级。'
        pacman -S --needed --noconfirm openssh
        ;;
esac

# 最小镜像的 PATH 可能不含 sbin。
PATH="${PATH:-/usr/bin:/bin}:/usr/sbin:/sbin"
export PATH
sshd_bin=$(command -v sshd) || fail '安装后仍未找到 sshd。'
command -v ssh-keygen >/dev/null 2>&1 || fail '缺少 ssh-keygen。'
[ -f "$config" ] && [ ! -L "$config" ] || fail "$config 必须是普通配置文件，不能是符号链接。"
ssh-keygen -A
mkdir -p /run/sshd

backup_dir=$(mktemp -d /etc/ssh/101cloud-backup.XXXXXXXX)
chmod 700 "$backup_dir"
backup="$backup_dir/sshd_config"
cp -p "$config" "$backup"
candidate=$(mktemp /etc/ssh/.sshd_config.XXXXXXXX)
chmod 600 "$candidate"
# sshd 的全局配置优先采用首次出现的值，故平台块必须位于 Include 前。
# 只替换本脚本自己的块，其余配置、Include 及 Match 限制原样保留。
# Port / ListenAddress 可重复；保留现有显式监听，但额外保证 IPv4 22 的配置。
awk -v root_policy="$root_policy" -v password_auth="$password_auth" '
BEGIN {
    print "# BEGIN 101cloud managed SSH access"
    print "Port 22"
    print "AddressFamily any"
    print "ListenAddress 0.0.0.0:22"
    print "PermitRootLogin " root_policy
    print "PasswordAuthentication " password_auth
    print "PubkeyAuthentication yes"
    print "PermitEmptyPasswords no"
    print "AuthenticationMethods any"
    print "# END 101cloud managed SSH access"
}
$0 == "# BEGIN 101cloud managed SSH access" { if (managed) exit 1; managed = 1; next }
$0 == "# END 101cloud managed SSH access" { if (!managed) exit 1; managed = 0; next }
!managed { print }
END { if (managed) exit 1 }
' "$config" > "$candidate" || fail '已有平台配置块不完整，拒绝覆盖。'

"$sshd_bin" -t -f "$candidate" || fail '新 SSH 配置语法检查失败，未替换原配置。'
# 检查真实有效配置，而不只检查文本；保留并显式拒绝冲突的 Match 策略。
effective=$("$sshd_bin" -T -f "$candidate" -C user=root,host=localhost,addr=127.0.0.1,laddr=127.0.0.1,lport=22) || fail '无法读取 SSH 有效配置。'
printf '%s\n' "$effective" | awk -v root_policy="$root_policy" -v password_auth="$password_auth" '
    { $1 = tolower($1) }
    $1 == "permitrootlogin" && ($2 == root_policy || (root_policy == "prohibit-password" && $2 == "without-password")) { root = 1 }
    $1 == "passwordauthentication" && $2 == password_auth { auth = 1 }
    $1 == "pubkeyauthentication" && $2 == "yes" { pubkey = 1 }
    $1 == "permitemptypasswords" && $2 == "no" { nonempty = 1 }
    $1 == "authenticationmethods" && $2 == "any" { standalone = 1 }
    $1 == "listenaddress" && $2 == "0.0.0.0:22" { listen = 1 }
    END { exit !(root && auth && pubkey && nonempty && standalone && listen) }
    ' || fail '所选 SSH 认证方式或 IPv4 22 监听被现有策略覆盖；请检查 Match / Include 配置。'

mv "$candidate" "$config"
candidate=
activated=1
log "配置备份：$backup"
if [ "$manager" = systemd ]; then
    # Ubuntu 的 ssh.socket 可独立持有监听套接字。切回 ssh 服务直接监听，
    # 避免只重启 ssh.service 而继续沿用旧 socket 的端口/地址。
    if systemctl cat ssh.socket >/dev/null 2>&1; then
        systemctl disable --now ssh.socket
    fi
    systemctl enable "$service_name.service"
    systemctl restart "$service_name.service"
    systemctl is-active --quiet "$service_name.service" || fail 'SSH 服务没有进入 active 状态。'
else
    update-rc.d "$service_name" defaults
    update-rc.d "$service_name" enable
    service "$service_name" restart
    service "$service_name" status >/dev/null 2>&1 || fail 'SSH 服务没有运行。'
fi

if command -v ss >/dev/null 2>&1; then
    listeners=$(ss -H -ltn) || fail '无法查询 TCP 监听状态。'
    printf '%s\n' "$listeners" | awk '$4 ~ /(^|:)22$/ { found = 1 } END { exit !found }' || fail '未检测到 TCP 22 监听。'
else
    log '提示：未安装 ss；已检查 sshd 有效配置和服务状态，未单独查询监听套接字。'
fi
complete=1
log "OpenSSH 已启用：IPv4 TCP 22，登录模式 $login_mode，禁止空密码。"
# 公钥模式不要求密码，也不应误报 root 没有密码。
# 不读取/输出 shadow 哈希，不更改用户已有密码或锁定状态。
if [ "$login_mode" = password ] && command -v passwd >/dev/null 2>&1; then
    root_status=$(passwd -S root 2>/dev/null || true)
    case "$root_status" in
        'root P '*|'root PS '*) ;;
        *) log '警告：root 尚无可用密码或账号已锁定；请通过受控流程设置密码（实例内 passwd root），否则密码登录仍不可用。' ;;
    esac
fi
log '现有 AllowUsers/AllowGroups、DenyUsers/DenyGroups、按来源的 Match 和 PAM 策略仍可能限制登录；本脚本不移除这些访问控制。'
