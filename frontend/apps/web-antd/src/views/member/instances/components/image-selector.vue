<script setup lang="ts">
import type { Image } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Select, Tag } from 'ant-design-vue';

import OsIcon from '#/components/os-icon.vue';
import { getOsType } from '#/utils/os-icons';

type Choice = Pick<Image, 'id' | 'os_name' | 'os_release' | 'name' | 'architecture' | 'gpu_ready'>;
const props = defineProps<{ images: Choice[]; gpuCount: number; disabled?: boolean }>();
const imageId = defineModel<string>();
const typeKey = ref('');
const compatible = (image: Choice) => props.gpuCount === 0 || image.gpu_ready;
const systems = computed(() => {
  const groups = new Map<string, { key: string; label: string; icon: string; images: Choice[] }>();
  for (const image of props.images) {
    const os = getOsType(image.os_name);
    let group = groups.get(os.key);
    if (!group) { group = { ...os, images: [] }; groups.set(os.key, group); }
    group.images.push(image);
  }
  return [...groups.values()];
});
const selectedSystem = computed(() => systems.value.find((system) => system.key === typeKey.value));
const selectedVersion = computed(() => selectedSystem.value?.images.find((image) => image.id === imageId.value));
const versions = computed(() => selectedSystem.value?.images.map((image) => ({
  value: image.id, label: `${image.os_release || '版本未提供'} · ${image.architecture}`,
  disabled: !compatible(image), image,
})) ?? []);
watch([() => props.images, () => props.gpuCount, imageId], () => {
  const current = props.images.find((image) => image.id === imageId.value && compatible(image));
  if (current) { typeKey.value = getOsType(current.os_name).key; return; }
  if (!selectedSystem.value) typeKey.value = (systems.value.find((system) => system.images.some(compatible)) ?? systems.value[0])?.key ?? '';
  imageId.value = selectedSystem.value?.images.find(compatible)?.id;
}, { immediate: true, flush: 'sync' });
function selectSystem(key: string) {
  if (props.disabled) return;
  typeKey.value = key;
  imageId.value = selectedSystem.value?.images.find(compatible)?.id;
}
</script>

<template>
  <div class="space-y-4">
    <div>
      <p class="mb-2 text-sm font-medium">系统类型</p>
      <div class="system-grid" role="group" aria-label="选择系统类型">
        <button v-for="system in systems" :key="system.key" type="button" class="system-option" :class="{ 'is-selected': typeKey === system.key }" :aria-pressed="typeKey === system.key" :disabled="disabled || !system.images.some(compatible)" @click="selectSystem(system.key)">
          <OsIcon :name="system.icon" class="text-[32px]" />
          <span class="break-words text-sm font-medium">{{ system.label }}</span>
        </button>
      </div>
    </div>
    <div v-if="selectedSystem">
      <label for="creation-system-version" class="mb-2 block text-sm font-medium">系统版本</label>
      <Select id="creation-system-version" v-model:value="imageId" class="w-full" :aria-label="`${selectedSystem.label} 系统版本`" :disabled="disabled" :options="versions" placeholder="请选择系统版本">
        <template #option="{ image, label }">
          <div class="flex items-center gap-2">
            <span class="min-w-0 flex-1"><strong class="block break-words text-sm">{{ label }}</strong><span class="text-muted-foreground block break-words text-xs">{{ image.name }}</span><span v-if="!compatible(image)" class="text-destructive block text-xs">不支持当前 GPU 配置</span></span>
            <Tag v-if="image.gpu_ready" class="m-0 shrink-0">GPU 就绪</Tag>
          </div>
        </template>
      </Select>
      <p v-if="selectedVersion" class="text-muted-foreground mb-0 mt-2 break-words text-xs">{{ selectedVersion.name }}</p>
      <p v-if="!selectedSystem.images.some(compatible)" class="text-destructive mb-0 mt-2 text-sm" role="alert">此系统没有支持当前 GPU 配置的版本，请选择其他系统或将 GPU 数量设为 0。</p>
    </div>
  </div>
</template>

<style scoped>
.system-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 6.5rem), 1fr)); gap: 0.6rem; }
.system-option { width: 100%; padding: 0.8rem; border: 1px solid hsl(var(--border)); border-radius: 0.65rem; background: hsl(var(--card)); color: inherit; cursor: pointer; transition: border-color 150ms ease, background-color 150ms ease; }
.system-option { display: flex; min-height: 96px; flex-direction: column; align-items: center; justify-content: center; gap: 0.5rem; }
.system-option:hover:not(:disabled), .is-selected { border-color: hsl(var(--primary)); background: hsl(var(--primary) / 0.05); }
.system-option:focus-visible { outline: 2px solid hsl(var(--primary)); outline-offset: 2px; }
.system-option:disabled { cursor: not-allowed; opacity: 0.6; }
@media (prefers-reduced-motion: reduce) { .system-option { transition: none; } }
</style>
