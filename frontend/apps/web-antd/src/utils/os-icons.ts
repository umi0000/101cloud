const SYSTEM_ICONS: Readonly<Record<string, string>> = {
  alma: 'alma',
  almalinux: 'alma',
  alpine: 'alpine',
  alpinelinux: 'alpine',
  arch: 'arch',
  archlinux: 'arch',
  centos: 'centos',
  centosstream: 'centos',
  debian: 'debian',
  fedora: 'fedora',
  freebsd: 'freebsd',
  gentoo: 'gentoo',
  kalilinux: 'kali',
  kali: 'kali',
  linux: 'linux',
  linuxmint: 'mint',
  mint: 'mint',
  nixos: 'nixos',
  opensuse: 'opensuse',
  opensuseleap: 'opensuse',
  opensusetumbleweed: 'opensuse',
  openwrt: 'openwrt',
  rocky: 'rocky',
  rockylinux: 'rocky',
  ubuntu: 'ubuntu',
  ubuntulinux: 'ubuntu',
};

const SYSTEM_NAMES: Readonly<Record<string, string>> = {
  alma: 'AlmaLinux', alpine: 'Alpine Linux', arch: 'Arch Linux', centos: 'CentOS',
  debian: 'Debian', fedora: 'Fedora', freebsd: 'FreeBSD', gentoo: 'Gentoo',
  kali: 'Kali Linux', linux: 'Linux', mint: 'Linux Mint', nixos: 'NixOS',
  opensuse: 'openSUSE', openwrt: 'OpenWrt', rocky: 'Rocky Linux', ubuntu: 'Ubuntu',
};

/** Group aliases without merging unrecognized distributions into generic Linux. */
export function getOsType(name?: null | string): { key: string; label: string; icon: string } {
  const raw = (name ?? '').trim();
  const normalized = raw.toLowerCase().replace(/[\s_-]+/g, '') || 'linux';
  const key = Object.hasOwn(SYSTEM_ICONS, normalized) ? SYSTEM_ICONS[normalized]! : normalized;
  return { key, label: Object.hasOwn(SYSTEM_NAMES, key) ? SYSTEM_NAMES[key]! : raw, icon: raw || 'linux' };
}

/** Match OS family, independent of release. Unknown systems use the Linux icon. */
export function getOsIcon(name?: null | string): string {
  const key = (name ?? '').trim().toLowerCase().replace(/[\s_-]+/g, '');
  const icon = Object.hasOwn(SYSTEM_ICONS, key) ? SYSTEM_ICONS[key] : 'linux';
  return `${import.meta.env.BASE_URL}images/os/${icon}.svg`;
}
