<script lang="ts" setup>
import type { InitTemplate } from '#/api/types';

import { onMounted, ref } from 'vue';

import { Button, Space, Table } from 'ant-design-vue';

import { catalogApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useAction, useCursorList } from '#/composables/api';

import TemplateDrawer from './template-drawer.vue';

const emit = defineEmits<{ changed: [] }>();

const PHASE_NAMES: Record<string, string> = { cloud_init: 'cloud_init（等 cloud-init 完成）', post_start: 'post_start（启动后）' };

const list = useCursorList<InitTemplate>(catalogApi.templates, ref({}));
const action = useAction();

const columns = [
  { dataIndex: 'id', title: 'ID', width: 60 },
  { key: 'code', title: '代码 / 版本' },
  { dataIndex: 'name', title: '名称' },
  { key: 'phase', title: '阶段' },
  { key: 'timeout', title: '超时', width: 90 },
  { key: 'status', title: '状态', width: 80 },
  { key: 'updated', title: '更新', width: 120 },
  { key: 'actions', title: '', width: 190 },
];

const drawerOpen = ref(false);
const drawerMode = ref<'create' | 'edit' | 'view'>('create');
const drawerSource = ref<InitTemplate | null>(null);

function openDrawer(mode: 'create' | 'edit' | 'view', t: InitTemplate | null) {
  drawerMode.value = mode;
  drawerSource.value = t;
  drawerOpen.value = true;
}

async function reload() {
  await list.load();
  emit('changed');
}

const target = ref<InitTemplate | null>(null);
const publishOpen = ref(false);
const retireOpen = ref(false);

async function publish() {
  const t = target.value;
  if (!t) return false;
  const r = await action.run((key) => catalogApi.publishTemplate(t.id, { ifMatch: revisionTag(t.revision), key }), {
    onConflict: () => list.load(),
    success: '模板已发布',
  });
  await reload();
  return !!r;
}

async function retire() {
  const t = target.value;
  if (!t) return false;
  const r = await action.run(
    (key) => catalogApi.retireTemplate(t.id, { ifMatch: revisionTag(t.revision), key }),
    { onConflict: () => list.load(), success: '模板已退役' },
  );
  await reload();
  return !!r;
}

onMounted(() => list.load());
</script>

<template>
  <div>
    <Space class="mb-3">
      <Button type="primary" @click="openDrawer('create', null)">新建初始化模板</Button>
      <span class="text-muted-foreground text-xs">模板脚本在实例首次启动后于实例内执行。草稿可修改；发布后只读，可被套餐和自定义实例引用。</span>
    </Space>
    <Table
      :columns="columns"
      :data-source="list.items.value"
      :loading="list.loading.value"
      :pagination="false"
      row-key="id"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.dataIndex === 'id'"><span class="font-mono">{{ record.id }}</span></template>
        <template v-else-if="column.key === 'code'">
          <span class="font-mono">{{ record.code }} v{{ record.version }}</span>
        </template>
        <template v-else-if="column.key === 'phase'">{{ PHASE_NAMES[record.phase] ?? record.phase }}</template>
        <template v-else-if="column.key === 'timeout'">{{ record.timeout_seconds }} 秒</template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="catalogStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'updated'"><TimeText :value="record.updated_at" /></template>
        <template v-else-if="column.key === 'actions'">
          <template v-if="record.status === 'draft'">
            <Button size="small" type="link" @click="openDrawer('edit', record as InitTemplate)">编辑</Button>
            <Button
              size="small"
              type="link"
              @click="
                target = record as InitTemplate;
                publishOpen = true;
              "
            >
              发布
            </Button>
          </template>
          <template v-else>
            <Button size="small" type="link" @click="openDrawer('view', record as InitTemplate)">查看</Button>
            <Button size="small" type="link" @click="openDrawer('create', record as InitTemplate)">新版本</Button>
            <Button
              v-if="record.status === 'active'"
              danger
              size="small"
              type="link"
              @click="
                target = record as InitTemplate;
                retireOpen = true;
              "
            >
              退役
            </Button>
          </template>
        </template>
      </template>
    </Table>
    <CursorPager
      :has-next="list.hasNext()"
      :has-prev="list.hasPrev()"
      :loading="list.loading.value"
      :page="list.page()"
      @next="list.nextPage"
      @prev="list.prevPage"
      @refresh="list.load"
    />

    <TemplateDrawer v-model:open="drawerOpen" :mode="drawerMode" :source="drawerSource" @saved="reload" />

    <ConfirmAction
      v-model:open="publishOpen"
      description="发布前后端再次校验参数 schema。发布后脚本、schema、超时、阶段都不能修改，只能退役并新建版本。"
      :on-confirm="publish"
      :title="`发布模板 ${target?.code ?? ''} v${target?.version ?? ''}`"
    />
    <ConfirmAction
      v-model:open="retireOpen"
      danger
      description="退役后不能再被新套餐或新的自定义实例引用；已引用它的实例首次启动时仍会执行该脚本。"
      :on-confirm="retire"
      :title="`退役模板 ${target?.code ?? ''} v${target?.version ?? ''}`"
    />
  </div>
</template>
