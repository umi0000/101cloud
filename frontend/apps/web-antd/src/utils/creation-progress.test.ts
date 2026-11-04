import type { Task } from '#/api/types';

import { describe, expect, it } from 'vitest';

import { CreationProgressSchema, TaskSchema } from '#/api/task-schema';

import { CREATION_STAGES, canEnterCreatedInstance, creationState, elapsedLabel } from './creation-progress';

function task(overrides: Partial<Task> = {}): Task {
  return {
    created_at: '2026-11-03T00:00:00Z', started_at: '2026-11-03T00:00:01Z',
    finished_at: '2026-11-03T00:01:00Z', id: 'task-a', instance_id: 'instance-a',
    error_code: null, next_action: '', node_id: 'node-a', phase: 'verify', revision: '1',
    status: 'succeeded', steps: [], type: 'instance.create', user_id: 'user-a',
    creation_progress: {
      initialization_pending: false,
      stages: CREATION_STAGES.map(({ key }) => ({ key, status: key === 'gpu' || key === 'script' ? 'skipped' : 'succeeded' })),
    },
    ...overrides,
  };
}

describe('authoritative creation progress', () => {
  it('requires completed server stages and a cleared initialization flag', () => {
    expect(creationState(task())).toBe('succeeded');
    const stopped = task();
    stopped.creation_progress!.initialization_pending = true;
    expect(creationState(stopped)).toBe('pending');
    expect(creationState(task({ creation_progress: undefined }))).toBe('unavailable');
    expect(creationState(task({ type: 'instance.start' }))).toBe('unavailable');
    expect(creationState(task({ status: 'running' }))).toBe('active');
  });

  it('does not infer success from missing or unverified stages', () => {
    const incomplete = task();
    incomplete.creation_progress!.stages.pop();
    expect(creationState(incomplete)).toBe('unavailable');
    const skippedVerify = task();
    skippedVerify.creation_progress!.stages.at(-1)!.status = 'skipped';
    expect(creationState(skippedVerify)).toBe('unavailable');
  });

  it('distinguishes failures, unknown results and cancellation', () => {
    for (const status of ['failed', 'unknown', 'cancelled'] as const) expect(creationState(task({ status }))).toBe(status);
    for (const status of ['failed', 'unknown'] as const) {
      const result = task();
      result.creation_progress!.stages[0]!.status = status;
      expect(creationState(result)).toBe(status);
    }
    expect(creationState(null)).toBe('loading');
  });

  it('keeps an unknown task outcome despite a known failed initialization stage', () => {
    const result = task({ status: 'unknown' });
    result.creation_progress!.stages[4]!.status = 'failed';
    expect(creationState(result)).toBe('unknown');
    expect(result.creation_progress!.stages[4]!.status).toBe('failed');
    expect(canEnterCreatedInstance(result, { id: 'instance-a', creation_task_id: 'task-a', user_id: 'user-a' }, 'user-a')).toBe(false);
  });

  it('only enters the instance owned by this account and linked to this creation task', () => {
    const instance = { id: 'instance-a', creation_task_id: 'task-a', user_id: 'user-a' };
    expect(canEnterCreatedInstance(task(), instance, 'user-a')).toBe(true);
    expect(canEnterCreatedInstance(task(), instance, 'user-b')).toBe(false);
    expect(canEnterCreatedInstance(task(), { ...instance, id: 'other' }, 'user-a')).toBe(false);
    expect(canEnterCreatedInstance(task(), { ...instance, creation_task_id: 'other' }, 'user-a')).toBe(false);
    expect(canEnterCreatedInstance(task({ status: 'unknown' }), instance, 'user-a')).toBe(false);
    expect(canEnterCreatedInstance(task(), instance, null)).toBe(false);
  });

  it('validates stage statuses and rejects duplicate stage identifiers', () => {
    expect(TaskSchema.safeParse(task()).success).toBe(true);
    expect(CreationProgressSchema.safeParse({ initialization_pending: false, stages: [{ key: 'script', status: 'complete' }] }).success).toBe(false);
    expect(CreationProgressSchema.safeParse({ initialization_pending: false, stages: [{ key: 'script', status: 'skipped' }, { key: 'script', status: 'succeeded' }] }).success).toBe(false);
  });

  it('uses timestamps for elapsed time without inventing durations', () => {
    expect(elapsedLabel('2026-11-03T00:00:00Z', '2026-11-03T00:01:05Z')).toBe('1 分 05 秒');
    expect(elapsedLabel(null, Date.now())).toBe('—');
    expect(elapsedLabel('invalid', Date.now())).toBe('—');
    expect(elapsedLabel('2026-11-03T00:01:00Z', '2026-11-03T00:00:00Z')).toBe('—');
  });
});
