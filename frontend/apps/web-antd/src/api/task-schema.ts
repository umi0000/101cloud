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


export const TaskSchema = z.object({
  created_at: z.string(),
  error: PublicErrorSchema.nullable().optional(),
  error_code: z.string().nullable(),
  finished_at: z.string().nullable(),
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
