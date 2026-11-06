<script lang="ts" setup>
/** 一次性显示密码并提供复制；关闭后调用方应丢弃密码。 */
import { useClipboard } from '@vueuse/core';
import { Alert, Button, Input, message, Modal, Space } from 'ant-design-vue';

defineProps<{ description: string; password: string; title: string }>();
const emit = defineEmits<{ closed: [] }>();
const open = defineModel<boolean>('open', { default: false });
const { copy: copyText, copied } = useClipboard({ legacy: true });

async function copy(text: string) {
  try {
    await copyText(text);
    if (!copied.value) throw new Error('Clipboard copy failed');
    message.success('已复制');
  } catch {
    message.error('复制失败，请手动选择复制');
  }
}
</script>

<template>
  <Modal
    v-model:open="open"
    :after-close="() => emit('closed')"
    :closable="false"
    :mask-closable="false"
    :title="title"
  >
    <Alert class="mb-3" :message="description" show-icon type="warning" />
    <Space.Compact class="w-full">
      <Input class="font-mono" readonly :value="password" />
      <Button type="primary" @click="copy(password)">复制</Button>
    </Space.Compact>
    <template #footer>
      <Button type="primary" @click="open = false">我已保存</Button>
    </template>
  </Modal>
</template>
