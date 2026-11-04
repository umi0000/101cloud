import type { App, Slots } from 'vue';
import type { Task } from '#/api/types';

import { createApp, h, nextTick } from 'vue';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const runtime = vi.hoisted(() => ({
  push: vi.fn(),
  tasks: { open: true, active: [], tracked: [] as Array<{ taskId: string; task: Task; label: string; requestId: string }>, clearFinished: vi.fn() },
}));
vi.mock('vue-router', () => ({ useRouter: () => ({ push: runtime.push }) }));
vi.mock('#/store/tasks', () => ({ useTaskStore: () => runtime.tasks }));
vi.mock('#/store/auth', () => ({ useAuthStore: () => ({ isAdmin: false }) }));
vi.mock('./error-feedback.vue', () => ({ default: () => null }));
vi.mock('./status-tag.vue', () => ({ default: () => null }));
vi.mock('./time-text.vue', () => ({ default: () => null }));
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', slots.default?.()) };
  return { Badge: container, Tooltip: container, Drawer: container, Button: 'button', Empty: () => null };
});

import TaskCenter from './task-center.vue';

let app: App;
let root: HTMLElement;
beforeEach(() => { runtime.push.mockReset(); runtime.tasks.open = true; runtime.tasks.tracked = []; });
afterEach(() => { app?.unmount(); root?.remove(); });

describe('failed creation task center entry', () => {
  it.each(['failed', 'unknown'] as const)('retains the initialization error route after %s creation cleanup completes', async (status) => {
    const task: Task = {
      id: 'create-task', instance_id: 'hidden-instance', user_id: 'user-a', type: 'instance.create', status,
      created_at: '2026-11-04T00:00:00Z', started_at: null, finished_at: null,
      error_code: null, phase: 'verify', revision: '1', next_action: 'inspect_failure', node_id: null, steps: [],
      creation_cleanup: { status: 'completed', billing_status: 'refunded', instance_name: 'safe retired name' },
    };
    runtime.tasks.tracked = [{ taskId: task.id, task, label: '创建实例', requestId: '' }];
    root = document.createElement('div');
    document.body.append(root);
    app = createApp(TaskCenter);
    app.mount(root);
    await nextTick();
    expect(root.textContent).not.toContain('再决定是否重新提交');
    expect(root.textContent).not.toContain('退款状态');
    const entry = [...root.querySelectorAll('button')].find((button) => button.textContent === '查看初始化');
    expect(entry).toBeDefined();
    entry!.click();
    expect(runtime.push).toHaveBeenCalledWith({ name: 'MemberInstanceInitialization', params: { taskId: task.id } });
    expect(runtime.tasks.open).toBe(false);
    expect(task.status).toBe(status);
  });
});
