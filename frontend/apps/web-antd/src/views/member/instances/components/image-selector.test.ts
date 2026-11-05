import type { App } from 'vue';

import { createApp, h, nextTick, ref } from 'vue';
import { afterEach, describe, expect, it, vi } from 'vitest';

import ImageSelector from './image-selector.vue';

vi.mock('ant-design-vue', () => ({ Tag: 'span', Select: {
  props: ['value', 'options', 'disabled'], emits: ['update:value'],
  setup: (props: { value?: string; options: { value: string; label: string; disabled: boolean }[]; disabled: boolean }, { emit }: { emit: (name: string, value: string) => void }) => () => h('select', {
    value: props.value, disabled: props.disabled,
    onChange: (event: Event) => emit('update:value', (event.target as HTMLSelectElement).value),
  }, props.options.map((option) => h('option', { value: option.value, disabled: option.disabled }, option.label))),
} }));
const image = (id: string, os_name: string, os_release: string, gpu_ready = true) => ({ id, os_name, os_release, gpu_ready, name: `${os_name}-${os_release}-${id}`, architecture: 'x86_64' });
const catalog = () => [image('u24', 'Ubuntu', '24.04'), image('u26', 'ubuntu-linux', '26.04'), image('arch', 'Archlinux', 'current', false), image('fedora', 'Fedora', '44')];
let app: App;
let root: HTMLElement;
function mount(initial?: string) {
  const images = ref(catalog());
  const selected = ref(initial);
  const gpuCount = ref(0);
  const disabled = ref(false);
  root = document.createElement('div'); document.body.append(root);
  app = createApp({ setup: () => () => h(ImageSelector, { images: images.value, modelValue: selected.value, 'onUpdate:modelValue': (id?: string) => { selected.value = id; }, gpuCount: gpuCount.value, disabled: disabled.value }) });
  app.mount(root);
  return { images, selected, gpuCount, disabled };
}
const system = (name: string) => [...root.querySelectorAll<HTMLButtonElement>('.system-option')].find((button) => button.textContent?.trim() === name)!;
const versions = () => [...root.querySelectorAll<HTMLOptionElement>('option')];
afterEach(() => { app?.unmount(); root?.remove(); });

describe('OS family then exact image version selection', () => {
  it('groups aliases and submits an exact version, not a family label', async () => {
    const state = mount('u24'); await nextTick();
    expect(root.querySelectorAll('.system-option')).toHaveLength(3);
    expect(versions()).toHaveLength(2);
    const dropdown = root.querySelector('select')!;
    dropdown.value = 'u26'; dropdown.dispatchEvent(new Event('change')); await nextTick();
    expect(state.selected.value).toBe('u26');
    system('Arch Linux').click(); await nextTick();
    expect(state.selected.value).toBe('arch');
    expect(versions()).toHaveLength(1);
    expect(root.querySelector('select')?.value).toBe('arch');
  });
  it('does not silently switch family when GPU eligibility changes', async () => {
    const state = mount('arch'); await nextTick();
    state.gpuCount.value = 1; await nextTick();
    expect(state.selected.value).toBeUndefined();
    expect(system('Arch Linux').getAttribute('aria-pressed')).toBe('true');
    expect(versions()[0]?.disabled).toBe(true);
    system('Fedora').click(); await nextTick();
    expect(state.selected.value).toBe('fedora');
  });
  it('restores the exact chosen image and keeps it across catalog refreshes', async () => {
    const state = mount('u26'); await nextTick();
    expect(system('Ubuntu').getAttribute('aria-pressed')).toBe('true');
    state.images.value = [...state.images.value].reverse(); await nextTick();
    expect(state.selected.value).toBe('u26');
    state.images.value = state.images.value.filter((entry) => entry.id !== 'u26'); await nextTick();
    expect(state.selected.value).toBe('u24');
    expect(system('Ubuntu').getAttribute('aria-pressed')).toBe('true');
    state.images.value = state.images.value.filter((entry) => entry.os_name === 'Fedora'); await nextTick();
    expect(state.selected.value).toBe('fedora');
  });
  it('keeps unknown distributions separate and respects locked inputs', async () => {
    const state = mount(); await nextTick();
    state.images.value = [image('a', 'Custom A', '1'), image('b', 'Custom B', '1')]; await nextTick();
    expect(root.querySelectorAll('.system-option')).toHaveLength(2);
    state.disabled.value = true; await nextTick();
    const before = state.selected.value;
    system('Custom B').click(); await nextTick();
    expect(state.selected.value).toBe(before);
    expect(root.querySelector('select')?.disabled).toBe(true);
  });
});
