<script lang="ts" setup>
/**
 * 终端 / 控制台，调用方显式传入当前角色的会话 API。
 * 容器 terminal（xterm）；虚拟机 vga（noVNC，默认）或 serial（xterm）。
 * 流程：POST 会话（幂等键、实例 ETag）→ 每秒查询至 ready（最多 30 秒）→ WebSocket（子协议 = protocol）。
 * 关闭：ws.close(1000) → GET 会话取 ETag → DELETE；404/410 忽略。不记录终端内容。
 */
import type { IDisposable } from '@xterm/xterm';

import type RFB from '@novnc/novnc';

import type { ConsoleKind, ConsoleSession, Instance } from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import { computed, nextTick, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import { Alert, Button, Modal, RadioButton, RadioGroup, Spin } from 'ant-design-vue';

import type { consoleApi } from '#/api/backend';
import { ApiError, newIdempotencyKey, websocketURL } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import { errorToast, useAction } from '#/composables/api';
import { enumLabel } from '#/locales/enums';
import { describeError } from '#/locales/errors';

const props = defineProps<{ api: typeof consoleApi; etag: null | string; instance: Instance; disabled?: boolean }>();

type Phase = 'connected' | 'connecting' | 'ended' | 'failed' | 'preparing';

const isVM = computed(() => props.instance.instance_type === 'virtual-machine');
const confirmOpen = ref(false);
const drawerOpen = ref(false);
const kind = ref<ConsoleKind>('terminal');
const phase = ref<Phase>('preparing');
const notice = ref<ErrorText | string>('');
const session = shallowRef<ConsoleSession | null>(null);
const termEl = ref<HTMLDivElement>();
const vncEl = ref<HTMLDivElement>();
const createAction = useAction();
const maximized = ref(false);
const fontSize = ref(14);
const phaseLabel = computed(() => ({
  preparing: '准备中', connecting: '连接中', connected: '已连接', ended: '已结束', failed: '连接失败',
})[phase.value]);
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
  if (props.disabled || !props.instance.capabilities.console) return;
  kind.value = isVM.value ? 'vga' : 'terminal';
  confirmOpen.value = true;
}
defineExpose({ start });

async function onConfirm() {
  if (props.disabled || !props.instance.capabilities.console) return false;
  maximized.value = false;
  drawerOpen.value = true;
  void connect();
  return true;
}

/** 浮窗内容异步挂载：等到终端/VNC 容器出现（最多约 1 秒）。 */
async function waitForElements(): Promise<boolean> {
  for (let n = 0; n < 60; n++) {
    await nextTick();
    if (termEl.value && vncEl.value) return true;
    await new Promise((resolve) => window.requestAnimationFrame(resolve));
  }
  return false;
}

const title = computed(() => {
  const name = kind.value === 'vga' ? 'VGA 控制台' : kind.value === 'serial' ? '串口控制台' : '终端';
  return `${name} · ${props.instance.name}`;
});

/* ---------- xterm ---------- */

function ensureTerminal(): null | Terminal {
  if (term) return term;
  const el = termEl.value;
  if (!el) return null;
  term = new Terminal({
    cursorBlink: true,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
    fontSize: fontSize.value,
    lineHeight: 1.2,
    theme: {
      background: '#090b0d', foreground: '#d5dbe3', cursor: '#e2e8f0',
      cursorAccent: '#090b0d', selectionBackground: '#334155',
      black: '#20252d', red: '#f87171', green: '#4ade80', yellow: '#facc15',
      blue: '#60a5fa', magenta: '#c084fc', cyan: '#22d3ee', white: '#d5dbe3',
      brightBlack: '#7c8798', brightRed: '#fca5a5', brightGreen: '#86efac',
      brightYellow: '#fde047', brightBlue: '#93c5fd', brightMagenta: '#d8b4fe',
      brightCyan: '#67e8f9', brightWhite: '#f8fafc',
    },
  });
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
  if (props.disabled || !props.instance.capabilities.console) return;
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
    props.api.create(props.instance.id, body, { ifMatch: props.etag, key }),
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
      s = (await props.api.get(s.session_id)).data;
      if (mine !== gen) return;
      session.value = s;
    }
  } catch (error) {
    return fail(mine, describeError(error));
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
  1008: '登录会话已失效或权限已变化',
  1011: '后端或上游连接失败',
};

function onClose(mine: number, code: number) {
  if (mine !== gen) return;
  phase.value = 'ended';
  if (!notice.value) notice.value = CLOSE_TEXT[code] ?? `连接已断开（${code}）`;
  for (const d of inputs) d.dispose();
  inputs = [];
}

function fail(mine: number, text: ErrorText | string) {
  if (mine !== gen) return;
  phase.value = 'failed';
  notice.value = text;
}

/** 关闭会话：先 GET 取 ETag 再 DELETE；已结束或 404/410 不处理。并发结束导致 412 时重查一次。 */
async function closeSession(s: ConsoleSession) {
  const terminal = (st: string) => st === 'closed' || st === 'expired' || st === 'failed';
  for (let attempt = 0; attempt < 2; attempt++) {
    try {
      const r = await props.api.get(s.session_id);
      if (terminal(r.data.status)) return;
      await props.api.close(s.session_id, { ifMatch: r.etag, key: newIdempotencyKey() });
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

watch(() => props.disabled, (disabled) => {
  if (!disabled) return;
  confirmOpen.value = false;
  drawerOpen.value = false;
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

function changeFontSize() {
  if (!term) return;
  term.options.fontSize = fontSize.value;
  fit?.fit();
  term.focus();
}

function clearScreen() {
  term?.clear();
  term?.focus();
}

async function toggleMaximized() {
  maximized.value = !maximized.value;
  await nextTick();
  fit?.fit();
}
</script>

<template>
  <ConfirmAction
    v-model:open="confirmOpen"
    description="控制台会话会记入审计。"
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

  <Modal
    v-model:open="drawerOpen"
    centered
    class="console-modal"
    :closable="false"
    :footer="null"
    :keyboard="false"
    :mask-closable="false"
    :mask-style="{ backgroundColor: 'rgb(0 0 0 / 55%)', backdropFilter: 'blur(3px)' }"
    :width="maximized ? 'calc(100vw - 24px)' : 'min(960px, calc(100vw - 32px))'"
    :aria-label="title"
    destroy-on-close
  >
    <template #title>
      <div class="console-toolbar">
        <div class="console-tab" :title="title">
          <span class="console-dot" :data-phase="phase" aria-hidden="true"></span>
          <span class="console-tab-kind">{{ kind === 'vga' ? 'VGA' : kind === 'serial' ? '串口' : '终端' }} 1</span>
          <span class="console-instance">{{ props.instance.name }}</span>
        </div>
        <div class="console-tools">
          <span class="console-status" :data-phase="phase" role="status">{{ phaseLabel }}</span>
          <select v-if="kind !== 'vga'" v-model.number="fontSize" class="console-font" aria-label="终端字号" title="终端字号" @change="changeFontSize">
            <option v-for="size in [12, 14, 16, 18, 20]" :key="size" :value="size">{{ size }}px</option>
          </select>
          <button v-if="kind !== 'vga'" class="console-tool" type="button" title="清屏（不执行命令）" aria-label="清屏" @click="clearScreen">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7M14 10v7" /></svg>
          </button>
          <button class="console-tool" type="button" :title="maximized ? '还原窗口' : '放大窗口'" :aria-label="maximized ? '还原窗口' : '放大窗口'" :aria-pressed="maximized" @click="toggleMaximized">
            <svg v-if="maximized" viewBox="0 0 24 24" aria-hidden="true"><path d="M8 3v5H3m18 0h-5V3M3 16h5v5m8 0v-5h5" /></svg>
            <svg v-else viewBox="0 0 24 24" aria-hidden="true"><path d="M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5" /></svg>
          </button>
          <button class="console-tool console-close" type="button" title="关闭终端" aria-label="关闭终端" @click="drawerOpen = false">
            <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m6 6 12 12M18 6 6 18" /></svg>
          </button>
        </div>
      </div>
    </template>
    <div class="console-panel" :class="{ 'console-panel-maximized': maximized }">
      <Alert
        v-if="notice"
        banner
        :message="typeof notice === 'string' ? notice : notice.text"
        :type="phase === 'failed' ? 'error' : phase === 'ended' ? 'warning' : 'info'"
      >
        <template v-if="typeof notice !== 'string'" #description>
          <ErrorFeedback :feedback="notice" hide-message />
        </template>
      </Alert>
      <div v-if="phase === 'ended' || phase === 'failed' || (kind === 'vga' && phase === 'connected')" class="console-actions">
        <Button v-if="kind === 'vga' && phase === 'connected'" size="small" @click="sendCtrlAltDel">发送 Ctrl+Alt+Del</Button>
        <Button v-if="phase === 'ended' || phase === 'failed'" :loading="createAction.pending.value" size="small" @click="reconnect">重新连接</Button>
      </div>
      <div class="console-screen">
        <div v-show="kind !== 'vga'" ref="termEl" class="console-terminal"></div>
        <div v-show="kind === 'vga'" ref="vncEl" class="console-vnc"></div>
        <div v-if="phase === 'preparing' || phase === 'connecting'" class="console-loading" role="status">
          <Spin size="small" />
          <span>{{ phase === 'preparing' ? '正在准备控制台…' : '正在连接…' }}</span>
        </div>
      </div>
    </div>
  </Modal>
</template>

<style scoped>
:global(.console-modal) { padding-bottom: 0; max-width: none; }
:global(.console-modal .ant-modal-content) {
  padding: 0; overflow: hidden; border: 1px solid #292e35; border-radius: 8px;
  background: #090b0d; box-shadow: 0 24px 80px rgb(0 0 0 / 50%);
}
:global(.console-modal .ant-modal-header) { margin: 0; padding: 0; background: #101215; border-bottom: 1px solid #242930; }
.console-toolbar { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 7px 10px; color: #a9b2bf; font-size: 12px; font-weight: 400; }
.console-tab { display: flex; align-items: center; gap: 8px; min-width: 0; padding: 5px 9px; border: 1px solid #30363e; border-radius: 4px; background: #1b2026; }
.console-tab-kind { color: #e2e8f0; white-space: nowrap; }
.console-instance { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.console-dot { width: 6px; height: 6px; flex-shrink: 0; border-radius: 50%; background: #94a3b8; }
.console-dot[data-phase='connected'] { background: #34d399; }
.console-dot[data-phase='failed'] { background: #f87171; }
.console-dot[data-phase='preparing'], .console-dot[data-phase='connecting'] { background: #fbbf24; }
.console-tools { display: flex; align-items: center; gap: 5px; flex-shrink: 0; }
.console-status { margin-right: 5px; color: #a9b2bf; font-size: 11px; white-space: nowrap; }
.console-status[data-phase='connected'] { color: #5ee6b5; }
.console-status[data-phase='failed'] { color: #fca5a5; }
.console-font { height: 26px; padding: 0 4px; border: 1px solid #343b45; border-radius: 4px; background: #15191e; color: #d5dbe3; color-scheme: dark; cursor: pointer; font: inherit; }
.console-tool { display: inline-flex; align-items: center; justify-content: center; width: 28px; height: 28px; border: 0; border-radius: 4px; background: transparent; color: #a9b2bf; cursor: pointer; transition: color 150ms, background 150ms; }
.console-tool:hover { background: #292f38; color: #f8fafc; }
.console-close:hover { background: #492329; color: #fda4af; }
.console-tool:focus-visible, .console-font:focus-visible { outline: 2px solid #60a5fa; outline-offset: 2px; }
.console-tool svg { width: 15px; height: 15px; fill: none; stroke: currentColor; stroke-width: 1.6; stroke-linecap: round; stroke-linejoin: round; }
.console-panel { display: flex; flex-direction: column; height: min(78dvh, 820px); min-height: 0; }
.console-panel-maximized { height: calc(100dvh - 76px); }
.console-screen { position: relative; flex: 1; min-height: 0; background: #090b0d; }
.console-terminal, .console-vnc { position: absolute; inset: 0; overflow: hidden; }
.console-terminal { padding: 12px; }
.console-terminal :deep(.xterm-viewport) { scrollbar-width: thin; scrollbar-color: #3c4654 #090b0d; }
.console-loading { position: absolute; inset: 0; display: flex; align-items: center; justify-content: center; gap: 10px; color: #a9b2bf; font-size: 13px; background: #090b0d; }
.console-actions { display: flex; justify-content: flex-end; padding: 8px 12px; border-bottom: 1px solid #242930; }
@media (max-width: 600px) {
  .console-toolbar { flex-wrap: wrap; gap: 6px; padding: 8px; }
  .console-tab { flex: 1; }
  .console-instance { max-width: 140px; }
  .console-tools { margin-left: auto; }
  .console-panel { height: 72dvh; }
  .console-panel-maximized { height: calc(100dvh - 116px); }
  .console-terminal { padding: 8px; }
}
@media (prefers-reduced-motion: reduce) { .console-tool { transition: none; } }
</style>
