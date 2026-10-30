<script lang="ts" setup>
/**
 * 明文兑换码只展示这一次：数据只在父组件的 ref 中，关闭即清空；
 * 不写入 store、localStorage 或日志。CSV 在浏览器内生成。
 */
import type { RedemptionCode } from '#/api/types';

import { computed } from 'vue';

import { Alert, Button, message, Modal, Space, Table } from 'ant-design-vue';

import { formatFullTime, formatPt } from '#/utils/format';

const codes = defineModel<RedemptionCode[]>('codes', { required: true });

const open = computed({
  get: () => codes.value.length > 0,
  set: (v) => {
    if (!v) codes.value = [];
  },
});

const columns = [
  { key: 'code', title: '兑换码' },
  { key: 'grant', title: '面额', align: 'right' as const, width: 100 },
  { key: 'uses', title: '可用次数', width: 90 },
  { key: 'expires', title: '过期时间', width: 170 },
];

async function copyAll() {
  try {
    await navigator.clipboard.writeText(codes.value.map((c) => c.code ?? '').join('\n'));
    message.success(`已复制 ${codes.value.length} 个兑换码`);
  } catch {
    message.error('复制失败，请使用导出 CSV');
  }
}

function csvCell(value: number | string): string {
  const s = String(value);
  return /[",\r\n]/.test(s) ? `"${s.replaceAll('"', '""')}"` : s;
}

function downloadCsv() {
  const header = ['code', 'display_prefix', 'grant_pt', 'max_uses', 'valid_from', 'expires_at'];
  const rows = codes.value.map((c) =>
    [c.code ?? '', c.display_prefix, c.grant_pt, c.max_uses, c.valid_from, c.expires_at].map(csvCell).join(','),
  );
  const blob = new Blob([[header.join(','), ...rows].join('\r\n')], { type: 'text/csv;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = `redemption-codes-${Date.now()}.csv`;
  a.click();
  URL.revokeObjectURL(url);
}
</script>

<template>
  <Modal
    v-model:open="open"
    destroy-on-close
    :keyboard="false"
    :mask-closable="false"
    title="兑换码明文"
    :width="760"
  >
    <Alert
      class="mb-3"
      message="明文兑换码只显示这一次，关闭后无法再次查看。请先复制或导出 CSV。"
      show-icon
      type="warning"
    />
    <Table
      :columns="columns"
      :data-source="codes"
      :pagination="false"
      row-key="id"
      :scroll="{ y: 400 }"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'code'"><span class="font-mono select-all">{{ record.code }}</span></template>
        <template v-else-if="column.key === 'grant'"><span class="font-mono">{{ formatPt(record.grant_pt) }}</span></template>
        <template v-else-if="column.key === 'uses'"><span class="font-mono">{{ record.max_uses }}</span></template>
        <template v-else-if="column.key === 'expires'">{{ formatFullTime(record.expires_at) }}</template>
      </template>
    </Table>
    <template #footer>
      <Space>
        <Button @click="copyAll">复制全部</Button>
        <Button @click="downloadCsv">导出 CSV</Button>
        <Button type="primary" @click="open = false">我已保存，关闭</Button>
      </Space>
    </template>
  </Modal>
</template>
