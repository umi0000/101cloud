<script setup lang="ts">
import { getOsIcon } from '#/utils/os-icons';

withDefaults(defineProps<{
  decorative?: boolean;
  name?: null | string;
}>(), { decorative: true, name: '' });
</script>

<template>
  <img
    :src="getOsIcon(name)"
    :alt="decorative ? '' : `${name || 'Linux'} 图标`"
    class="os-icon"
    :aria-hidden="decorative ? true : undefined"
    :role="decorative ? undefined : 'img'"
  />
</template>

<style scoped>
.os-icon {
  display: inline-block;
  flex-shrink: 0;
  width: 1em;
  height: 1em;
  vertical-align: middle;
  object-fit: contain;
}
</style>
