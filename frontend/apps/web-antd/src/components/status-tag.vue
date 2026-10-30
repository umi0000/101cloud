<script lang="ts" setup>
import type { EnumLabel } from '#/locales/enums';

import { computed } from 'vue';

import { Tag } from 'ant-design-vue';

import { enumLabel } from '#/locales/enums';

const props = defineProps<{ kind?: string; label?: EnumLabel; value?: null | string }>();

const resolved = computed(() => props.label ?? enumLabel(props.kind ?? '', props.value));
</script>

<template>
  <Tag :bordered="false" :color="resolved.color" class="m-0">{{ resolved.text }}</Tag>
</template>
