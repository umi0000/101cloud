import { createApp, defineComponent, h, nextTick, ref } from 'vue';

import { describe, expect, it, vi } from 'vitest';

import MetricsPolicySelect from './metrics-policy-select.vue';

vi.mock('#/api/backend', () => ({
  policiesApi: { list: vi.fn() },
  listAll: async () => [
    { id: '17', code: 'metrics', version: '1', status: 'active', effective_at: '2020-01-01T00:00:00Z' },
  ],
}));
vi.mock('#/composables/api', () => ({ errorToast: vi.fn() }));
vi.mock('ant-design-vue', () => ({
  Select: defineComponent({
    props: ['value', 'options'],
    emits: ['update:value'],
    setup(_, { emit }) {
      return () => h('button', { onClick: () => emit('update:value', '17') }, 'Select policy');
    },
  }),
}));

describe('metrics policy validation', () => {
  it('validates the new selection before wrapper props have rendered, and still rejects missing or unknown IDs', async () => {
    const selected = ref<string>();
    const control = ref<InstanceType<typeof MetricsPolicySelect>>();
    const host = document.createElement('div');
    document.body.append(host);
    const app = createApp({
      setup: () => () => h(MetricsPolicySelect, {
        ref: control,
        modelValue: selected.value,
        'onUpdate:modelValue': (value: string | undefined) => { selected.value = value; },
      }),
    });
    app.mount(host);
    try {
      await nextTick();
      await nextTick();
      await expect(control.value!.validate()).rejects.toThrow('请选择已发布且已生效的监控策略');
      host.querySelector('button')!.click();
      // Form change validation runs synchronously, before the parent render flush.
      await expect(control.value!.validate()).resolves.toBeUndefined();
      selected.value = 'unknown';
      await expect(control.value!.validate()).rejects.toThrow('请选择已发布且已生效的监控策略');
    } finally {
      app.unmount();
      host.remove();
    }
  });
});
