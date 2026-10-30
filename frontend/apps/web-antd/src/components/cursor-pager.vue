<script lang="ts" setup>
import { Button, Space } from 'ant-design-vue';

defineProps<{ hasNext: boolean; hasPrev: boolean; loading?: boolean; page: number }>();
const emit = defineEmits<{ next: []; prev: []; refresh: [] }>();
</script>

<template>
  <div class="mt-3 flex items-center justify-end">
    <Space>
      <span class="text-muted-foreground text-xs">第 {{ page }} 页</span>
      <Button size="small" :disabled="!hasPrev || loading" @click="emit('prev')">上一页</Button>
      <Button size="small" :disabled="!hasNext || loading" @click="emit('next')">下一页</Button>
      <Button size="small" :loading="loading" @click="emit('refresh')">刷新</Button>
    </Space>
  </div>
</template>
