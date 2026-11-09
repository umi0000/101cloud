<script lang="ts" setup>
import type { EchartsUIType } from '@vben/plugins/echarts';
import type { DistributionSlice } from './distribution';
import { ref, watch } from 'vue';
import { EchartsUI, useEcharts } from '@vben/plugins/echarts';
import { Card, Empty } from 'ant-design-vue';
import { formatPt } from '#/utils/format';
import { distributionOption } from './distribution';

const props = defineProps<{
  title: string;
  slices: DistributionSlice[];
  available: boolean;
  loading: boolean;
  partial: boolean;
}>();
const chart = ref<EchartsUIType>();
const { renderEcharts } = useEcharts(chart);
watch([() => props.slices, () => props.partial, chart], () => {
  if (props.available && chart.value) void renderEcharts(distributionOption(props.slices, props.partial));
}, { flush: 'post' });
</script>

<template>
  <Card :title="title" size="small" :body-style="{ padding: '4px 12px' }" class="min-w-0">
    <div :aria-label="title" :aria-busy="loading" class="min-w-0">
      <EchartsUI v-if="available && slices.length" ref="chart" height="180px" class="w-full min-w-0" aria-hidden="true" />
      <Empty v-else :description="loading ? '正在加载分布' : !available ? '分布暂不可用' : partial ? '暂无可展示的已确认实例' : '此窗口没有符合条件的实例'" class="flex flex-col items-center justify-center" :style="{ height: '180px', margin: 0 }" />
      <dl v-if="available && slices.length" class="sr-only" :aria-label="`${title}金额`">
        <div v-for="item in slices" :key="item.id">
          <dt>{{ item.label }}</dt>
          <dd>{{ formatPt(item.consumed) }} Pt</dd>
          <dd v-if="item.recorded !== undefined">已记录 {{ formatPt(item.recorded) }} Pt · 未结算尾段估算 {{ formatPt(item.estimated) }} Pt</dd>
        </div>
      </dl>
    </div>
  </Card>
</template>
