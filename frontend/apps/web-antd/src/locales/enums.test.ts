import { describe, expect, it } from 'vitest';

import { instanceTaskState } from './enums';

describe('instance active task presentation', () => {
  it('treats cancelled as stopped orchestration with an external reconciliation barrier', () => {
    const state = instanceTaskState({
      active_task_id: '90',
      active_task_status: 'cancelled',
    });
    expect(state).toMatchObject({
      color: 'warning',
      text: '编排已停，待核对',
      type: 'warning',
    });
    expect(state?.message).toContain('需管理员核对外部状态');
    expect(state?.message).toContain('资源与计费不视为已释放');
    expect(state?.message).not.toMatch(/操作中|正在处理/);
  });

  it.each(['queued', 'running', 'waiting'] as const)(
    'only describes live %s orchestration as processing',
    (status) => {
      expect(
        instanceTaskState({ active_task_id: '90', active_task_status: status }),
      ).toMatchObject({ text: '操作中', type: 'info' });
    },
  );

  it.each(['unknown', 'failed', 'succeeded', null, undefined] as const)(
    'preserves conservative reconciliation copy for retained or legacy %s status',
    (status) => {
      const state = instanceTaskState({
        active_task_id: '90',
        active_task_status: status,
      });
      expect(state).toMatchObject({ text: '操作结果待核对', type: 'warning' });
      expect(state?.message).toContain('安全控制仍禁用');
      expect(state?.message).not.toContain('正在处理');
    },
  );

  it.each(['cancelled', 'running', null, undefined] as const)(
    'does not create a safety barrier from %s status without an active ID',
    (status) => {
      expect(
        instanceTaskState({ active_task_id: null, active_task_status: status }),
      ).toBeUndefined();
    },
  );
});
