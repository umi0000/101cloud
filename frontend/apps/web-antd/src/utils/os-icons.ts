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

/** Match OS family, independent of release. Unknown systems use the Linux icon. */
export function getOsIcon(name?: null | string): string {
  const key = (name ?? '').trim().toLowerCase().replace(/[\s_-]+/g, '');
  const icon = Object.hasOwn(SYSTEM_ICONS, key) ? SYSTEM_ICONS[key] : 'linux';
  return `${import.meta.env.BASE_URL}images/os/${icon}.svg`;
}
