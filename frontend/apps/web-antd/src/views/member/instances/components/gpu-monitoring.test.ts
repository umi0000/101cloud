import type { App, Slots } from 'vue';
import type {
  GPUAllocation,
  Instance,
  LatestMetrics,
  MetricQuality,
  MetricsHistory,
} from '#/api/types';

import { createApp, h, nextTick, ref } from 'vue';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const runtime = vi.hoisted(() => ({
  latest: vi.fn(),
  history: vi.fn(),
  gpuAllocations: vi.fn(),
  gpuPolicy: vi.fn(),
  render: vi.fn(),
  poll: null as null | (() => unknown),
}));
vi.mock('@vben/stores', () => ({
  useAccessStore: () => ({ loginExpired: false }),
}));
vi.mock('#/store/auth', () => ({
  useAuthStore: () => ({ me: { user: { id: '2', role: 'user' } } }),
}));
vi.mock('#/api/backend', () => ({
  memberApi: {
    latest: runtime.latest,
    history: runtime.history,
    gpuAllocations: runtime.gpuAllocations,
    gpuPolicy: runtime.gpuPolicy,
  },
}));
vi.mock('#/composables/api', () => ({
  usePolling: (callback: () => unknown) => {
    runtime.poll = callback;
  },
  useAction: () => ({
    pending: ref(false),
    resetKey: vi.fn(),
    run: async (callback: () => unknown) => callback(),
  }),
  trackAccepted: vi.fn(),
}));
vi.mock('@vben/plugins/echarts', () => ({
  EchartsUI: { setup: () => () => h('div', { 'data-chart': '' }) },
  useEcharts: () => ({ renderEcharts: runtime.render }),
}));
vi.mock('echarts/core', () => ({ use: vi.fn() }));
vi.mock('echarts/components', () => ({ MarkAreaComponent: {} }));
vi.mock('ant-design-vue', () => {
  const container = {
    setup:
      (_: unknown, { slots }: { slots: Slots }) =>
      () =>
        h('div', slots.default?.()),
  };
  return {
    ...Object.fromEntries(
      [
        'Card',
        'Spin',
        'Collapse',
        'CollapsePanel',
        'Descriptions',
        'DescriptionsItem',
        'Form',
        'FormItem',
      ].map((key) => [key, container]),
    ),
    Button: 'button',
    Tag: 'span',
    Input: 'input',
    InputNumber: 'input',
    Switch: 'input',
    Alert: {
      props: ['message', 'description'],
      setup: (props: { message: string; description: string }) => () =>
        h('p', { role: 'status' }, [props.message, props.description]),
    },
    Empty: {
      props: ['description'],
      setup: (props: { description: string }) => () =>
        h('p', { 'data-empty': '' }, props.description),
    },
    Select: {
      props: ['value', 'options'],
      emits: ['update:value'],
      setup:
        (
          props: { value: string; options: { value: string; label: string }[] },
          { emit }: { emit: (name: string, value: string) => void },
        ) =>
        () =>
          h(
            'select',
            {
              value: props.value,
              onChange: (event: Event) =>
                emit('update:value', (event.target as HTMLSelectElement).value),
            },
            props.options.map((option) =>
              h('option', { value: option.value }, option.label),
            ),
          ),
    },
    Table: {
      props: ['columns', 'dataSource'],
      setup:
        (
          props: {
            columns: { key?: string; dataIndex?: string }[];
            dataSource: GPUAllocation[];
          },
          { slots }: { slots: Slots },
        ) =>
        () =>
          h(
            'div',
            { 'data-table': '' },
            props.dataSource.map((record) =>
              h(
                'div',
                props.columns.map((column) =>
                  slots.bodyCell?.({ column, record }),
                ),
              ),
            ),
          ),
    },
  };
});
vi.mock('#/components/confirm-action.vue', () => ({
  default: {
    props: ['open', 'disabled'],
    setup: (props: { open: boolean; disabled: boolean }) => () =>
      props.open
        ? h('button', { 'data-confirm': '', disabled: props.disabled }, '确认')
        : null,
  },
}));
vi.mock('#/components/error-feedback.vue', () => ({
  default: {
    props: ['feedback'],
    setup: (props: { feedback: unknown }) => () =>
      props.feedback ? h('p', { role: 'alert' }, '加载失败') : null,
  },
}));
vi.mock('#/components/time-text.vue', () => ({
  default: {
    props: ['value'],
    setup: (props: { value: string }) => () => h('time', props.value),
  },
}));

import GpuConfigPanel from './gpu-config-panel.vue';
import MetricsPanel from './metrics-panel.vue';

const quality = (patch: Partial<MetricQuality> = {}): MetricQuality => ({
  complete: true,
  attribution_complete: true,
  reference_scan_quality: 'complete',
  reasons: [],
  ...patch,
});
const latest = (): LatestMetrics => ({
  instance_id: '10',
  instance_state: 'running',
  sampled_at: '2026-11-05T10:00:00+08:00',
  age_seconds: '1',
  stale: false,
  collection_complete: true,
  gpu_state: 'idle_candidate',
  quality: quality(),
  cpu: { allocation_percent: 12, window_seconds: 10 },
  memory: { used_bytes: '1024', limit_bytes: '2048', peak_bytes: null },
  gpus: [
    {
      uuid: 'GPU-test',
      model: 'Test GPU',
      configured: true,
      observed: true,
      state: 'idle_candidate',
      process_count: 0,
      referencing_process_count: 0,
      used_memory_bytes: '0',
      sm_utilization_percent: 0,
      quality: quality(),
    },
  ],
});
const idleWithoutSamples = () => {
  const sample = latest();
  sample.quality = quality({
    complete: false,
    reasons: ['gpu_utilization_no_samples'],
  });
  sample.gpus[0] = {
    ...sample.gpus[0]!,
    sm_utilization_percent: null,
    quality: quality({
      complete: false,
      reasons: ['gpu_utilization_no_samples'],
    }),
  };
  return sample;
};
const history = (withGPU = false): MetricsHistory => ({
  available_range: null,
  from: '2026-11-05T09:00:00+08:00',
  to: '2026-11-05T10:00:00+08:00',
  resolution: 'raw',
  bucket_seconds: 10,
  coverage: { fraction: '1', seconds: '10' },
  gaps: [],
  collection_gaps: [],
  next_cursor: null,
  items: withGPU
    ? [
        {
          sampled_at: '2026-11-05T09:30:00+08:00',
          cpu: { allocation_percent: 20 },
          memory: { used_bytes: '1024' },
          gpus: [
            {
              uuid: 'GPU-test',
              gpu_model: 'Test GPU',
              sm_utilization_percent: null,
              used_memory_bytes: '180355072',
            },
          ],
        },
      ]
    : [],
});
const allocation: GPUAllocation = {
  id: '20',
  uuid: 'GPU-test',
  model: 'Test GPU',
  status: 'active',
  configured: true,
  observed: true,
  release_verified: false,
  released_at: null,
  created_at: '2026-11-05T09:00:00+08:00',
  revision: '1',
  gpu_hourly_pt: '1',
  gpu_price_version_id: null,
};
const instance = (): Instance =>
  ({
    id: '10',
    user_id: '2',
    instance_type: 'container',
    observed_state: 'running',
    provisioning_state: 'ready',
    initialization_pending: false,
    state_stale: false,
    active_task_id: null,
    revision: '1',
    credit_enforcement: { status: 'normal' },
    capabilities: { gpu_attach: true, gpu_rebind: true },
  }) as Instance;
const policy = () => ({
  gpu_binding: {
    model: 'Test GPU',
    access_mode: 'shared',
    gpu_hourly_pt: '1',
    policy_version_id: '1',
  },
  idle: { enabled: false, timeout_seconds: 60, action: 'detach_gpu' },
  rebalance: { enabled: false, allow_live: false, max_hourly_pt: '1' },
  effective_policy: {
    idle: { enabled: false, timeout_seconds: 60, action: 'detach_gpu' },
    rebalance: { enabled: false, allow_live: false, max_hourly_pt: '1' },
    safety: {},
  },
  allowed_user_actions: ['gpu_detach'],
  blocked_reasons: [],
  allowed_ranges: {
    editable: {},
    idle: {
      enabled: true,
      min_timeout_seconds: 10,
      max_timeout_seconds: 3600,
      allowed_actions: ['detach_gpu'],
    },
    rebalance: { min_hourly_pt: '0', max_hourly_pt: '10' },
  },
});
let app: App;
let root: HTMLElement;
async function flush() {
  for (let n = 0; n < 20; n++) await Promise.resolve();
  await nextTick();
}
function mount(kind: 'config' | 'metrics', value = instance()) {
  root = document.createElement('div');
  document.body.append(root);
  app = createApp({
    setup: () => () =>
      kind === 'config'
        ? h(GpuConfigPanel, { instance: value, etag: '"1"', active: true })
        : h(MetricsPanel, { instanceId: '10' }),
  });
  app.mount(root);
}
const button = (label: string) =>
  [...root.querySelectorAll<HTMLButtonElement>('button')].find(
    (item) => item.textContent === label,
  )!;
const warningTexts = () =>
  [...root.querySelectorAll('[color="warning"]')].map(
    (item) => item.textContent ?? '',
  );
async function selectAllocation() {
  root
    .querySelector<HTMLInputElement>('input[aria-label="选择 GPU 分配 20"]')!
    .click();
  await nextTick();
}
beforeEach(() => {
  vi.clearAllMocks();
  runtime.poll = null;
  runtime.latest.mockResolvedValue({ data: latest() });
  runtime.history.mockResolvedValue({ data: history() });
  runtime.gpuAllocations.mockResolvedValue({ data: [allocation] });
  runtime.gpuPolicy.mockResolvedValue({ data: policy(), etag: '"1"' });
});
afterEach(() => {
  app?.unmount();
  root?.remove();
});

describe('GPU configuration active task safety barrier', () => {
  it.each(['cancelled', 'unknown', 'running', undefined] as const)(
    'keeps %s controls disabled with accurate task copy',
    async (status) => {
      mount('config', {
        ...instance(),
        active_task_id: '90',
        active_task_status: status,
      });
      await flush();
      expect(button('挂载 GPU').disabled).toBe(true);
      await selectAllocation();
      expect(button('卸载所选').disabled).toBe(true);
      expect(button('换卡').disabled).toBe(true);
      expect(root.textContent).toContain('GPU 变更已暂停，请先核对上方任务。');
      expect(root.textContent).not.toMatch(
        /任务编排已停止|已有任务进行中|正在处理/,
      );
    },
  );

  it('returns to independently observed safe detach permissions without an active task', async () => {
    mount('config', { ...instance(), active_task_status: null });
    await flush();
    await selectAllocation();
    expect(button('卸载所选').disabled).toBe(false);
    expect(button('挂载 GPU').disabled).toBe(true);
    expect(root.textContent).not.toContain('GPU 变更已暂停');
  });

  it('retains policy loading failure feedback without an active task', async () => {
    runtime.gpuPolicy.mockRejectedValue(
      new Error('isolated policy unavailable'),
    );
    mount('config');
    await flush();
    expect(root.textContent).toContain('GPU 策略尚未成功加载，请刷新。');
    expect(root.textContent).not.toContain('GPU 变更已暂停');
  });
});

describe('independent instance GPU metric display', () => {
  it('shows known memory and processes on both layouts without dropping reference or SM warnings', async () => {
    const sample = latest();
    sample.quality = quality({
      complete: false,
      attribution_complete: false,
      reference_scan_quality: 'partial',
      reasons: ['gpu_metric_unavailable'],
      error_code: 'instance_attribution_unknown',
    });
    sample.gpus[0] = {
      ...sample.gpus[0]!,
      process_count: 1,
      used_memory_bytes: '180355072',
      sm_utilization_percent: null,
      referencing_process_count: null,
      quality: quality({
        complete: false,
        attribution_complete: false,
        reference_scan_quality: 'partial',
        reasons: ['gpu_utilization_unknown'],
      }),
    };
    runtime.latest.mockResolvedValue({ data: sample });
    mount('config');
    await flush();
    for (const selector of ['.gpu-desktop', '.gpu-mobile']) {
      const text = root.querySelector(selector)?.textContent;
      expect(text).toContain('172 MiB');
      expect(text).toContain('GPU 进程 1');
      expect(text).toContain('SM 未知');
      expect(text).toContain('设备引用 未知');
      expect(text).toContain('部分 GPU 指标不可用');
      expect(text).toContain('实例 GPU SM 利用率未知');
      expect(text).toContain('实例 GPU 占用归因尚未完整确认');
    }
    await selectAllocation();
    expect(button('卸载所选').disabled).toBe(true);
  });

  it('keeps known zero values distinct from unavailable memory and process fields', async () => {
    const sample = latest();
    sample.gpus[0] = {
      ...sample.gpus[0]!,
      used_memory_bytes: null,
      process_count: null,
      sm_utilization_percent: 0,
      referencing_process_count: 0,
    };
    runtime.latest.mockResolvedValue({ data: sample });
    mount('config');
    await flush();
    expect(root.querySelector('.gpu-mobile')?.textContent).toContain(
      'SM 0% · 归因显存 未知 · GPU 进程 未知 · 设备引用 0',
    );
    await selectAllocation();
    expect(button('卸载所选').disabled).toBe(true);
  });

  it('shows the same partial fields and human-readable reasons in monitoring', async () => {
    const sample = latest();
    sample.quality = quality({
      complete: false,
      reasons: ['gpu_metric_unavailable'],
    });
    sample.gpus[0] = {
      ...sample.gpus[0]!,
      process_count: 1,
      referencing_process_count: null,
      sm_utilization_percent: null,
      used_memory_bytes: '180355072',
      quality: quality({
        complete: false,
        attribution_complete: false,
        reasons: ['gpu_utilization_unknown'],
        error_code: 'instance_attribution_unknown',
      }),
    };
    runtime.latest.mockResolvedValue({ data: sample });
    mount('metrics');
    await flush();
    expect(root.textContent).toContain('归因显存：172 MiB');
    expect(root.textContent).toContain('SM：未知');
    expect(root.textContent).toContain('GPU 进程数：1');
    expect(root.textContent).toContain('设备引用数：未知');
    expect(root.textContent).toContain('部分 GPU 指标不可用');
    expect(root.textContent).toContain('实例 GPU SM 利用率未知');
    expect(root.textContent).toContain('实例 GPU 占用归因尚未完整确认');
  });

  it('explains stopped samples without filling unknowns or dropping historical GPU data', async () => {
    const sample = latest();
    sample.instance_state = 'stopped';
    sample.gpus = [];
    sample.quality = quality({
      complete: false,
      attribution_complete: false,
      reasons: ['instance_attribution_unknown'],
    });
    runtime.latest.mockResolvedValue({ data: sample });
    runtime.history.mockResolvedValue({ data: history(true) });
    mount('metrics');
    await flush();
    expect(root.textContent).toContain('实例已停止，没有运行态 GPU 占用采样');
    expect(root.textContent).toContain('未知不表示占用为 0；历史监控仍可查看');
    expect(
      root.querySelector('select[aria-label="选择监控 GPU"] option')
        ?.textContent,
    ).toContain('历史');
    const gpuOptions = runtime.render.mock.calls
      .map(([option]) => option)
      .filter((option) => option.series[0].name.startsWith('所选 GPU'));
    expect(
      gpuOptions.find(
        (option) => option.series[0].name === '所选 GPU SM 利用率',
      )?.series[0].data[0][1],
    ).toBeNull();
    expect(
      gpuOptions.find((option) => option.series[0].name === '所选 GPU 归因显存')
        ?.series[0].data[0][1],
    ).toBe(180355072 / 1024 ** 3);
  });

  it('does not turn stopped configured-GPU unknown fields into zero', async () => {
    const sample = latest();
    sample.instance_state = 'stopped';
    sample.gpus[0] = {
      ...sample.gpus[0]!,
      observed: false,
      process_count: null,
      referencing_process_count: null,
      sm_utilization_percent: null,
      used_memory_bytes: null,
      quality: quality({
        complete: false,
        attribution_complete: false,
        reasons: ['instance_attribution_unknown'],
      }),
    };
    runtime.latest.mockResolvedValue({ data: sample });
    mount('metrics');
    await flush();
    expect(root.textContent).toContain('实例已停止，没有运行态 GPU 占用采样');
    expect(root.textContent).toContain('SM：未知');
    expect(root.textContent).toContain('归因显存：未知');
    expect(root.textContent).toContain('GPU 进程数：未知');
    expect(root.textContent).toContain('设备引用数：未知');
    expect(root.textContent).not.toContain('GPU 进程数：0');
    expect(root.textContent).not.toContain('归因显存：0');
  });

  it('does not report absent GPU data before latest loading finishes or after it fails', async () => {
    const pending = Promise.withResolvers<unknown>();
    runtime.latest.mockReturnValue(pending.promise);
    mount('metrics');
    await flush();
    expect(root.textContent).toContain('GPU 数据尚未完整取得');
    expect(root.textContent).not.toContain('均无 GPU 数据');
    pending.reject(new Error('offline'));
    await flush();
    expect(root.textContent).toContain('加载失败');
    expect(root.textContent).not.toContain('均无 GPU 数据');
  });

  it('does not claim both data sources are empty when history loading fails', async () => {
    const sample = latest();
    sample.gpus = [];
    runtime.latest.mockResolvedValue({ data: sample });
    runtime.history.mockRejectedValue(new Error('offline'));
    mount('metrics');
    await flush();
    expect(root.textContent).toContain('加载失败');
    expect(root.textContent).toContain('GPU 数据尚未完整取得');
    expect(root.textContent).not.toContain('均无 GPU 数据');
  });

  it('keeps historical GPU availability separate from a failed latest request', async () => {
    runtime.latest.mockRejectedValue(new Error('offline'));
    runtime.history.mockResolvedValue({ data: history(true) });
    mount('metrics');
    await flush();
    expect(root.textContent).toContain('暂无最新 GPU 采样，当前占用未知');
    expect(root.textContent).not.toContain('所选 GPU 在最新采样中不存在');
    expect(
      root.querySelector('select[aria-label="选择监控 GPU"] option')
        ?.textContent,
    ).toContain('历史');
  });
});

describe.each(['config', 'metrics'] as const)(
  '%s normal GPU sampling states',
  (kind) => {
    it('shows mounted idle GPU evidence and neutral no-samples reasons without inventing SM zero', async () => {
      runtime.latest.mockResolvedValue({ data: idleWithoutSamples() });
      mount(kind);
      await flush();
      expect(root.textContent).toContain(
        'GPU已挂载，尚无GPU程序活动；运行GPU任务后显示利用率',
      );
      expect(root.textContent).toContain('暂无进程级SM活动采样');
      expect(root.textContent).toContain('暂无活动采样');
      expect(root.textContent).toContain('采样状态说明');
      expect(root.textContent).not.toMatch(
        /部分指标不可用|归因不完整|SM[： ]0%/,
      );
      expect(warningTexts()).toEqual([]);
      if (kind === 'config') {
        for (const selector of ['.gpu-desktop', '.gpu-mobile']) {
          expect(root.querySelector(selector)?.textContent).toContain(
            'GPU已挂载，尚无GPU程序活动',
          );
          expect(root.querySelector(selector)?.textContent).toContain(
            '归因显存 0 B · GPU 进程 0 · 设备引用 0',
          );
        }
        await selectAllocation();
        expect(button('卸载所选').disabled).toBe(true);
      } else {
        expect(root.textContent).toContain('归因显存：0 B');
        expect(root.textContent).toContain('GPU 进程数：0');
        expect(root.textContent).toContain('设备引用数：0');
      }
    });

    it.each<[string, (sample: LatestMetrics) => void]>([
      [
        'unconfigured GPU',
        (sample) => {
          sample.gpus[0]!.configured = false;
        },
      ],
      [
        'stale sample',
        (sample) => {
          sample.stale = true;
        },
      ],
      [
        'non-running sample',
        (sample) => {
          sample.instance_state = 'unknown';
        },
      ],
      [
        'GPU context',
        (sample) => {
          sample.gpus[0]!.process_count = 1;
        },
      ],
      [
        'allocated memory',
        (sample) => {
          sample.gpus[0]!.used_memory_bytes = '1';
        },
      ],
      [
        'device reference',
        (sample) => {
          sample.gpus[0]!.referencing_process_count = 1;
        },
      ],
      [
        'unknown processes',
        (sample) => {
          sample.gpus[0]!.process_count = null;
        },
      ],
      [
        'unknown memory',
        (sample) => {
          sample.gpus[0]!.used_memory_bytes = null;
        },
      ],
      [
        'unknown references',
        (sample) => {
          sample.gpus[0]!.referencing_process_count = null;
        },
      ],
    ])(
      'does not infer mounted idle activity from no samples with %s',
      async (_name, mutate) => {
        const sample = idleWithoutSamples();
        mutate(sample);
        runtime.latest.mockResolvedValue({ data: sample });
        mount(kind);
        await flush();
        expect(root.textContent).toContain('暂无进程级SM活动采样');
        expect(root.textContent).not.toContain('GPU已挂载，尚无GPU程序活动');
      },
    );

    it('keeps unsupported active utilization visibly unknown and warning-colored', async () => {
      const sample = latest();
      sample.quality = quality({
        complete: false,
        reasons: ['gpu_metric_unavailable'],
      });
      sample.gpus[0] = {
        ...sample.gpus[0]!,
        process_count: 1,
        used_memory_bytes: '180355072',
        sm_utilization_percent: null,
        quality: quality({
          complete: false,
          reasons: ['gpu_utilization_unknown'],
        }),
      };
      runtime.latest.mockResolvedValue({ data: sample });
      mount(kind);
      await flush();
      expect(root.textContent).toContain('172 MiB');
      expect(root.textContent).toMatch(/SM[： ]未知/);
      expect(root.textContent).not.toContain('暂无活动采样');
      expect(root.textContent).not.toContain('GPU已挂载，尚无GPU程序活动');
      expect(warningTexts()).toContain('实例 GPU SM 利用率未知');
    });

    it('does not label a null SM value as no activity from aggregate reasons alone', async () => {
      const sample = idleWithoutSamples();
      sample.gpus[0]!.quality = quality();
      runtime.latest.mockResolvedValue({ data: sample });
      mount(kind);
      await flush();
      expect(root.textContent).toMatch(/SM[： ]未知/);
      expect(root.textContent).not.toMatch(/SM[： ]暂无活动采样/);
      expect(warningTexts()).toContain('部分 GPU 活动指标未知');
      expect(root.textContent).not.toContain('采样状态说明');
    });

    it('keeps stopped unknown quantities visible with neutral state reasons', async () => {
      const sample = latest();
      sample.instance_state = 'stopped';
      sample.gpu_state = 'unknown';
      sample.quality = quality({
        complete: false,
        attribution_complete: false,
        reference_scan_quality: 'unknown',
        reasons: ['instance_not_running'],
      });
      sample.gpus[0] = {
        ...sample.gpus[0]!,
        state: 'unknown',
        observed: false,
        process_count: null,
        used_memory_bytes: null,
        referencing_process_count: null,
        sm_utilization_percent: null,
        quality: { ...sample.quality },
      };
      runtime.latest.mockResolvedValue({ data: sample });
      mount(kind);
      await flush();
      expect(root.textContent).toContain('实例已停止，没有运行态 GPU 占用采样');
      expect(root.textContent).toContain('实例已停止，无运行态进程归因');
      expect(root.textContent).toContain('采样状态说明');
      expect(root.textContent).toMatch(/SM[： ]未知/);
      expect(root.textContent).toMatch(/归因显存[： ]未知/);
      expect(root.textContent).toMatch(/GPU 进程(?:数：| )未知/);
      expect(root.textContent).toMatch(/设备引用(?:数：| )未知/);
      expect(root.textContent).not.toContain('暂无活动采样');
      expect(root.textContent).not.toContain('GPU已挂载，尚无GPU程序活动');
      expect(warningTexts()).toEqual([]);
    });

    it.each<[string, (sample: LatestMetrics) => void, string]>([
      [
        'historical attribution failure',
        (sample) => {
          sample.quality.error_code = 'instance_attribution_unknown';
        },
        '实例 GPU 占用归因尚未完整确认',
      ],
      [
        'unknown error code',
        (sample) => {
          sample.quality.error_code = 'nvml_failed';
        },
        'nvml_failed',
      ],
      [
        'unknown reason',
        (sample) => {
          sample.gpus[0]!.quality.reasons.push('unexpected_gpu_failure');
        },
        'unexpected_gpu_failure',
      ],
      [
        'invalid process field',
        (sample) => {
          sample.gpus[0]!.process_count = null;
        },
        '部分 GPU 活动指标未知',
      ],
    ])(
      'does not let normal reasons hide %s',
      async (_name, mutate, expectedWarning) => {
        const sample = idleWithoutSamples();
        mutate(sample);
        runtime.latest.mockResolvedValue({ data: sample });
        mount(kind);
        await flush();
        expect(root.textContent).toContain('暂无进程级SM活动采样');
        expect(warningTexts()).toContain(expectedWarning);
        expect(root.textContent).not.toContain('采样状态说明');
        expect(warningTexts()).not.toContain('暂无进程级SM活动采样');
      },
    );
  },
);

describe('live GPU release remains fail-closed', () => {
  it('allows a selected allocation only with fresh complete empty ownership and references', async () => {
    mount('config');
    await flush();
    await selectAllocation();
    expect(button('卸载所选').disabled).toBe(false);
    button('卸载所选').click();
    await nextTick();
    expect(
      root.querySelector<HTMLButtonElement>('[data-confirm]')?.disabled,
    ).toBe(false);
    const sample = latest();
    sample.stale = true;
    runtime.latest.mockResolvedValue({ data: sample });
    await runtime.poll?.();
    await flush();
    expect(button('卸载所选').disabled).toBe(true);
    expect(
      root.querySelector<HTMLButtonElement>('[data-confirm]')?.disabled,
    ).toBe(true);
  });

  it.each<[string, (sample: LatestMetrics) => void]>([
    [
      'stale sampling',
      (sample) => {
        sample.stale = true;
      },
    ],
    [
      'non-running sample',
      (sample) => {
        sample.instance_state = 'stopped';
      },
    ],
    [
      'incomplete collection',
      (sample) => {
        sample.collection_complete = false;
      },
    ],
    [
      'incomplete aggregate quality',
      (sample) => {
        sample.quality.complete = false;
      },
    ],
    [
      'incomplete aggregate attribution',
      (sample) => {
        sample.quality.attribution_complete = false;
      },
    ],
    [
      'partial aggregate references',
      (sample) => {
        sample.quality.reference_scan_quality = 'partial';
      },
    ],
    [
      'missing aggregate reference quality',
      (sample) => {
        delete sample.quality.reference_scan_quality;
      },
    ],
    [
      'incomplete GPU quality',
      (sample) => {
        sample.gpus[0]!.quality.complete = false;
      },
    ],
    [
      'incomplete GPU attribution',
      (sample) => {
        sample.gpus[0]!.quality.attribution_complete = false;
      },
    ],
    [
      'partial GPU references',
      (sample) => {
        sample.gpus[0]!.quality.reference_scan_quality = 'partial';
      },
    ],
    [
      'missing GPU reference quality',
      (sample) => {
        delete sample.gpus[0]!.quality.reference_scan_quality;
      },
    ],
    [
      'unknown GPU processes',
      (sample) => {
        sample.gpus[0]!.process_count = null;
      },
    ],
    [
      'unknown device references',
      (sample) => {
        sample.gpus[0]!.referencing_process_count = null;
      },
    ],
    [
      'unknown GPU memory',
      (sample) => {
        sample.gpus[0]!.used_memory_bytes = null;
      },
    ],
    [
      'nonzero GPU processes',
      (sample) => {
        sample.gpus[0]!.process_count = 1;
      },
    ],
    [
      'nonzero device references',
      (sample) => {
        sample.gpus[0]!.referencing_process_count = 1;
      },
    ],
    [
      'nonzero GPU memory',
      (sample) => {
        sample.gpus[0]!.used_memory_bytes = '1';
      },
    ],
    [
      'not idle candidate',
      (sample) => {
        sample.gpus[0]!.state = 'unknown';
      },
    ],
    [
      'missing matching GPU',
      (sample) => {
        sample.gpus = [];
      },
    ],
  ])(
    'blocks release for %s without using relaxed display eligibility',
    async (_name, mutate) => {
      const sample = latest();
      mutate(sample);
      runtime.latest.mockResolvedValue({ data: sample });
      mount('config');
      await flush();
      await selectAllocation();
      expect(button('卸载所选').disabled).toBe(true);
    },
  );

  it('blocks live release when the latest request fails', async () => {
    runtime.latest.mockRejectedValue(new Error('offline'));
    mount('config');
    await flush();
    await selectAllocation();
    expect(button('卸载所选').disabled).toBe(true);
    expect(root.textContent).toContain('暂无可用 GPU 采样；占用未知');
  });
});
