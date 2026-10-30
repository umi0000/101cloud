<script lang="ts" setup>
import { computed, watchEffect } from 'vue';

import { useUserCache } from '#/store/user-cache';

const props = defineProps<{ id?: null | string; link?: boolean }>();
const cache = useUserCache();

watchEffect(() => {
  if (props.id) cache.ensure(props.id);
});

const name = computed(() => (props.id ? cache.names[props.id] : undefined));
</script>

<template>
  <span v-if="!id" class="text-muted-foreground">系统</span>
  <RouterLink v-else-if="link" :to="`/admin/users/${id}`">{{ name ?? `#${id}` }}</RouterLink>
  <span v-else>{{ name ?? `#${id}` }}</span>
</template>
