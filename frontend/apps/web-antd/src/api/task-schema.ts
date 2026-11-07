import { z } from 'zod';

import { PublicErrorSchema } from './request';

/** 任务查询是异步错误的输入边界，不把未知 JSON 直接断言为 Task。 */
export const TaskStatusSchema = z.enum([
  'cancelled', 'failed', 'queued', 'running', 'succeeded', 'unknown', 'waiting',
]);

export const TaskStepSchema = z.object({
  error: PublicErrorSchema.nullable().optional(),
  error_code: z.string().nullable(),
  finished_at: z.string().nullable(),
  name: z.string(),
  started_at: z.string().nullable(),
  status: TaskStatusSchema,
});

/** 表格插槽只声明未知行结构；渲染错误前校验任务与步骤共用字段。 */
export const TaskErrorSourceSchema = TaskStepSchema.pick({
  error: true,
  error_code: true,
  status: true,
});

export const CreationProgressSchema = z.object({
  initialization_pending: z.boolean(),
  stages: z.array(z.object({
    key: z.enum(['preflight', 'native', 'gpu', 'start', 'cloud_init', 'script', 'verify']),
    status: z.enum(['pending', 'running', 'succeeded', 'skipped', 'failed', 'unknown']),
    started_at: z.string().nullable().optional(),
    finished_at: z.string().nullable().optional(),
  })).superRefine((stages, context) => {
    if (new Set(stages.map((stage) => stage.key)).size !== stages.length) {
      context.addIssue({ code: z.ZodIssueCode.custom, message: '初始化阶段重复' });
    }
  }),
});

export const CreationCleanupSchema = z.object({
  status: z.enum(['pending', 'cleaning', 'unknown', 'completed']),
  billing_status: z.enum(['pending', 'refunded']),
  instance_name: z.string(),
});


export const TaskSchema = z.object({
  can_force_stop: z.boolean().optional(),
  force_stop_blocked_reason: z.string().optional(),
  creation_cleanup: CreationCleanupSchema.nullable().optional(),
  creation_progress: CreationProgressSchema.nullable().optional(),
  created_at: z.string(),
  error: PublicErrorSchema.nullable().optional(),
  error_code: z.string().nullable(),
  finished_at: z.string().nullable(),
  force_stop: z.object({
    orchestration_stopped: z.literal(true),
    external_state: z.enum(['not_dispatched', 'requires_reconciliation']),
    requires_reconciliation: z.boolean(),
    resources_retained: z.boolean().optional(),
    reconciliation_completed: z.boolean().optional(),
  }).nullable().optional(),
  id: z.string(),
  instance_id: z.string().nullable(),
  next_action: z.string(),
  node_id: z.string().nullable(),
  phase: z.string().nullable(),
  resource_reservations_released: z.boolean().optional(),
  result: z.record(z.unknown()).nullable().optional(),
  revision: z.string(),
  started_at: z.string().nullable(),
  status: TaskStatusSchema,
  steps: z.array(TaskStepSchema),
  type: z.string(),
  user_id: z.string().nullable(),
});

export const TaskPageSchema = z.object({
  items: z.array(TaskSchema),
  next_cursor: z.string().nullable(),
});
