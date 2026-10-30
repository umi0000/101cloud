<script lang="ts" setup>
/**
 * 终端 / 控制台（frontend/前端设计.md 6.5，管理端接口）。
 * 容器 terminal（xterm）；虚拟机 vga（noVNC，默认）或 serial（xterm）。
 * 流程：POST 会话（幂等键、实例 ETag）→ 每秒查询至 ready（最多 30 秒）→ WebSocket（子协议 = protocol）。
 * 关闭：ws.close(1000) → GET 会话取 ETag → DELETE；404/410 忽略。不记录终端内容。
 */
import type { IDisposable } from '@xterm/xterm';

import type RFB from '@novnc/novnc';

import type { ConsoleKind, ConsoleSession, Instance } from '#/api/types';

import { computed, nextTick, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import { Alert, Button, Drawer, RadioButton, RadioGroup, Space, Spin } from 'ant-design-vue';

import { consoleApi } from '#/api/backend';
import { ApiError, newIdempotencyKey, websocketURL } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import { errorToast, useAction } from '#/composables/api';
import { enumLabel } from '#/locales/enums';
import { describeError } from '#/locales/errors';

import { formatCountdown, useNow } from './shared';

const props = defineProps<{ etag: null | string; instance: Instance }>();

type Phase = 'connected' | 'connecting' | 'ended' | 'failed' | 'preparing';

const isVM = computed(() => props.instance.instance_type === 'virtual-machine');
const confirmOpen = ref(false);
const drawerOpen = ref(false);
const kind = ref<ConsoleKind>('terminal');
const phase = ref<Phase>('preparing');
const notice = ref('');
const session = shallowRef<ConsoleSession | null>(null);
const termEl = ref<HTMLDivElement>();
const vncEl = ref<HTMLDivElement>();
const now = useNow();
const createAction = useAction();
let gen = 0;

let socket: null | WebSocket = null;
let term: null | Terminal = null;
let fit: FitAddon | null = null;
let observer: null | ResizeObserver = null;
let rfb: null | RFB = null;
let inputs: IDisposable[] = [];
let fitTimer: number | undefined;

const encoder = new TextEncoder();

function start() {
  kind.value = isVM.value ? 'vga' : 'terminal';
  confirmOpen.value = true;
}
defineExpose({ start });

async function onConfirm() {
  drawerOpen.value = true;
  void connect();
  return true;
}

/** 抽屉内容异步挂载：等到终端/VNC 容器出现（最多约 1 秒）。 */
async function waitForElements(): Promise<boolean> {
  for (let n = 0; n < 60; n++) {
    await nextTick();
    if (termEl.value && vncEl.value) return true;
    await new Promise((resolve) => window.requestAnimationFrame(resolve));
  }
  return false;
}

const remaining = computed(() => {
  const s = session.value;
  if (!s) return null;
  const t = Date.parse(s.expires_at);
  return Number.isNaN(t) ? null : t - now.value;
});
const title = computed(() => {
  const name = kind.value === 'vga' ? 'VGA 控制台' : kind.value === 'serial' ? '串口控制台' : '终端';
  const r = remaining.value;
  return `${name} · ${props.instance.name}${r === null ? '' : ` · 剩余 ${formatCountdown(r)}`}`;
});

/* ---------- xterm ---------- */

function ensureTerminal(): null | Terminal {
  if (term) return term;
  const el = termEl.value;
  if (!el) return null;
  term = new Terminal({ cursorBlink: true, fontFamily: 'ui-monospace, Menlo, Consolas, monospace', fontSize: 14 });
  fit = new FitAddon();
  term.loadAddon(fit);
  term.open(el);
  fit.fit();
  term.onResize(({ cols, rows }) => {
    if (kind.value !== 'terminal' || socket?.readyState !== WebSocket.OPEN) return;
    socket.send(JSON.stringify({ cols, rows, type: 'resize' }));
  });
  observer = new ResizeObserver(() => {
    window.clearTimeout(fitTimer);
    fitTimer = window.setTimeout(() => fit?.fit(), 200);
  });
  observer.observe(el);
  return term;
}

function disposeTerminal() {
  window.clearTimeout(fitTimer);
  observer?.disconnect();
  observer = null;
  term?.dispose();
  term = null;
  fit = null;
}

const clamp = (n: number) => Math.min(1000, Math.max(1, n));

/* ---------- 会话 ---------- */

async function connect() {
  teardownConnection();
  const mine = ++gen;
  phase.value = 'preparing';
  notice.value = '';
  const k = kind.value;
  if (!(await waitForElements())) return fail(mine, '控制台初始化失败');
  if (mine !== gen) return;
  let t: null | Terminal = null;
  if (k !== 'vga') {
    t = ensureTerminal();
    if (!t) return fail(mine, '终端初始化失败');
    fit?.fit();
    t.reset();
  }
  const body =
    k === 'terminal' && t
      ? { cols: clamp(t.cols), kind: k, rows: clamp(t.rows) }
      : { kind: k };
  createAction.resetKey();
  const r = await createAction.run((key) =>
    consoleApi.create(props.instance.id, body, { ifMatch: props.etag, key }),
  );
  if (mine !== gen) {
    if (r) void closeSession(r.data);
    return;
  }
  if (!r) return fail(mine, '创建控制台会话失败');
  session.value = r.data;
  await waitReady(r.data, mine);
}

async function waitReady(initial: ConsoleSession, mine: number) {
  const deadline = Date.now() + 30_000;
  let s = initial;
  try {
    while (s.status === 'preparing') {
      if (Date.now() > deadline) return fail(mine, '控制台准备超时，请重新连接');
      await new Promise((resolve) => window.setTimeout(resolve, 1000));
      if (mine !== gen) return;
      s = (await consoleApi.get(s.session_id)).data;
      if (mine !== gen) return;
      session.value = s;
    }
  } catch (error) {
    return fail(mine, describeError(error).text);
  }
  if (s.status !== 'ready') return fail(mine, `会话${enumLabel('consoleStatus', s.status).text}，请重新连接`);
  await openSocket(s, mine);
}

async function openSocket(s: ConsoleSession, mine: number) {
  phase.value = 'connecting';
  const url = websocketURL(s.websocket_path);
  if (s.protocol === '101cloud.vnc.v1') {
    const { default: RFBClass } = await import('@novnc/novnc');
    if (mine !== gen) return;
    if (!vncEl.value) return fail(mine, '控制台初始化失败');
    const ws = new WebSocket(url, s.protocol);
    ws.binaryType = 'arraybuffer';
    socket = ws;
    ws.addEventListener('close', (ev) => onClose(mine, ev.code));
    const client = new RFBClass(vncEl.value, ws, { shared: true });
    client.scaleViewport = true;
    client.background = '#000';
    client.addEventListener('connect', () => {
      if (mine === gen) phase.value = 'connected';
    });
    client.addEventListener('securityfailure', () => {
      if (mine === gen) notice.value = 'VNC 认证失败';
    });
    rfb = client;
    return;
  }
  const t = term;
  if (!t) return fail(mine, '终端初始化失败');
  const ws = new WebSocket(url, s.protocol);
  ws.binaryType = 'arraybuffer';
  socket = ws;
  ws.addEventListener('open', () => {
    if (mine !== gen) return;
    phase.value = 'connected';
    term?.focus();
  });
  ws.addEventListener('message', (ev: MessageEvent<ArrayBuffer | string>) => {
    if (mine !== gen) return;
    if (typeof ev.data === 'string') handleControl(ev.data);
    else term?.write(new Uint8Array(ev.data));
  });
  ws.addEventListener('close', (ev) => onClose(mine, ev.code));
  const send = (bytes: Uint8Array<ArrayBuffer>) => {
    if (ws.readyState === WebSocket.OPEN) ws.send(bytes);
  };
  inputs = [
    t.onData((d) => send(encoder.encode(d))),
    t.onBinary((d) => send(Uint8Array.from(d, (c) => c.charCodeAt(0) & 0xff))),
  ];
}

function handleControl(text: string) {
  let msg: unknown;
  try {
    msg = JSON.parse(text);
  } catch {
    return;
  }
  if (!msg || typeof msg !== 'object') return;
  const m = msg as { code?: string; exit_code?: number; message?: string; type?: string };
  if (m.type === 'exit') {
    notice.value = `进程已退出（退出码 ${m.exit_code ?? '未知'}）`;
  } else if (m.type === 'error') {
    if (m.code === 'console_closed') notice.value = '会话已结束';
    else if (m.code === 'resize_unsupported') notice.value = '串口不支持调整尺寸';
    else notice.value = m.message || m.code || '控制台错误';
  }
}

const CLOSE_TEXT: Record<number, string> = {
  1000: '会话已结束',
  1008: '会话已过期或权限已变化',
  1011: '后端或上游连接失败',
};

function onClose(mine: number, code: number) {
  if (mine !== gen) return;
  phase.value = 'ended';
  if (!notice.value) notice.value = CLOSE_TEXT[code] ?? `连接已断开（${code}）`;
  for (const d of inputs) d.dispose();
  inputs = [];
}

function fail(mine: number, text: string) {
  if (mine !== gen) return;
  phase.value = 'failed';
  notice.value = text;
}

/** 关闭会话：先 GET 取 ETag 再 DELETE；已结束或 404/410 不处理。并发结束导致 412 时重查一次。 */
async function closeSession(s: ConsoleSession) {
  const terminal = (st: string) => st === 'closed' || st === 'expired' || st === 'failed';
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      const r = await consoleApi.get(s.session_id);
      if (terminal(r.data.status)) return;
      await consoleApi.close(s.session_id, { ifMatch: r.etag, key: newIdempotencyKey() });
      return;
    } catch (error) {
      if (error instanceof ApiError && (error.status === 404 || error.status === 410)) return;
      if (error instanceof ApiError && error.code === 'revision_conflict' && attempt === 0) continue;
      errorToast(error);
      return;
    }
  }
}

function teardownConnection() {
  gen++;
  for (const d of inputs) d.dispose();
  inputs = [];
  const ws = socket;
  socket = null;
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) ws.close(1000);
  rfb?.disconnect();
  rfb = null;
  const s = session.value;
  session.value = null;
  if (s) void closeSession(s);
}

watch(drawerOpen, (open) => {
  if (open) return;
  teardownConnection();
  disposeTerminal();
});

onBeforeUnmount(() => {
  teardownConnection();
  disposeTerminal();
});

function reconnect() {
  void connect();
}

function sendCtrlAltDel() {
  rfb?.sendCtrlAltDel();
}
</script>

<template>
  <ConfirmAction
    v-model:open="confirmOpen"
    description="管理员控制台会话会记入审计。"
    ok-text="连接"
    :on-confirm="onConfirm"
    title="打开控制台"
  >
    <div class="mb-3">
      <div class="mb-1 text-sm">类型</div>
      <RadioGroup v-if="isVM" v-model:value="kind" button-style="solid">
        <RadioButton value="vga">VGA（图形）</RadioButton>
        <RadioButton value="serial">串口</RadioButton>
      </RadioGroup>
      <span v-else class="text-sm">终端（容器）</span>
    </div>
  </ConfirmAction>

  <Drawer
    v-model:open="drawerOpen"
    :body-style="{ display: 'flex', flexDirection: 'column', padding: 0 }"
    destroy-on-close
    :title="title"
    width="100%"
  >
    <template #extra>
      <Space>
        <Button
          v-if="kind === 'vga' && phase === 'connected'"
          size="small"
          @click="sendCtrlAltDel"
        >
          发送 Ctrl+Alt+Del
        </Button>
        <Button
          v-if="phase === 'ended' || phase === 'failed'"
          :loading="createAction.pending.value"
          size="small"
          type="primary"
          @click="reconnect"
        >
          重新连接
        </Button>
      </Space>
    </template>
    <Alert
      v-if="notice"
      banner
      :message="notice"
      :type="phase === 'failed' ? 'error' : phase === 'ended' ? 'warning' : 'info'"
    />
    <div v-if="phase === 'preparing' || phase === 'connecting'" class="flex items-center gap-2 px-4 py-2 text-sm">
      <Spin size="small" />
      {{ phase === 'preparing' ? '正在准备控制台…' : '正在连接…' }}
    </div>
    <div class="relative min-h-0 flex-1 bg-black">
      <div v-show="kind !== 'vga'" ref="termEl" class="absolute inset-0 p-1"></div>
      <div v-show="kind === 'vga'" ref="vncEl" class="absolute inset-0"></div>
    </div>
  </Drawer>
</template>
