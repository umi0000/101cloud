<script lang="ts" setup>
import type { FormInstance } from 'ant-design-vue';

import type { NetworkPoolView } from './network';

import type { Zone } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, Button, Col, Drawer, Form, FormItem, Input, Row, Select } from 'ant-design-vue';

import { networkApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import { useAction } from '#/composables/api';

import { CIDR_RE, IP_RE } from './network';

const props = defineProps<{ pool: NetworkPoolView | null; zones: Zone[] }>();
const emit = defineEmits<{ saved: [] }>();
const open = defineModel<boolean>('open', { default: false });

interface PoolForm {
  cidr: string;
  dns_servers: string[];
  gateway: string;
  name: string;
  range_end: string;
  range_start: string;
  status: 'active' | 'disabled';
  zone_id: string | undefined;
}

const form = ref<PoolForm>(blank());
const formRef = ref<FormInstance>();
const action = useAction();
watch(form, () => action.resetKey(), { deep: true });

function blank(): PoolForm {
  const p = props.pool;
  return {
    cidr: p?.cidr ?? '',
    dns_servers: [...(p?.dns_servers ?? [])],
    gateway: p?.gateway ?? '',
    name: p?.name ?? '',
    range_end: p?.range_end ?? '',
    range_start: p?.range_start ?? '',
    status: p?.status === 'disabled' ? 'disabled' : 'active',
    zone_id: p?.availability_zone_id,
  };
}
watch(open, (v) => {
  if (v) form.value = blank();
});

const editing = computed(() => !!props.pool);
const sameList = (a: string[], b: string[]) => a.length === b.length && a.every((v, i) => v === b[i]);
const semanticsChanged = computed(() => {
  const p = props.pool;
  if (!p) return false;
  return form.value.gateway.trim() !== (p.gateway ?? '') || !sameList(form.value.dns_servers, p.dns_servers);
});

const ipRule = (label: string, required: boolean) => [
  { message: `请填写${label}`, required },
  { message: `${label}格式不正确`, pattern: IP_RE },
];
const rules = {
  cidr: [
    { message: '请填写网段', required: true },
    { message: '格式如 192.168.10.0/24', pattern: CIDR_RE },
  ],
  dns_servers: [
    {
      validator: async (_r: unknown, v: unknown) => {
        const list = Array.isArray(v) ? v : [];
        if (list.length > 16) throw new Error('最多 16 个 DNS');
        if (list.some((s) => typeof s !== 'string' || !IP_RE.test(s))) throw new Error('DNS 地址格式不正确');
      },
    },
  ],
  gateway: [{ message: '网关格式不正确', pattern: IP_RE }],
  name: [
    { message: '请填写名称', required: true, whitespace: true },
    { max: 128, message: '最多 128 字符' },
  ],
  range_end: ipRule('结束地址', true),
  range_start: ipRule('起始地址', true),
  zone_id: [{ message: '请选择可用区', required: true }],
};

async function submit() {
  try {
    await formRef.value?.validate();
  } catch {
    return;
  }
  const f = form.value;
  const p = props.pool;
  const dns = f.dns_servers.map((s) => s.trim());
  let ok = false;
  if (p) {
    const body: Partial<{ dns_servers: string[]; gateway: string; name: string; status: 'active' | 'disabled' }> = {};
    if (f.name.trim() !== p.name) body.name = f.name.trim();
    if (f.status !== p.status) body.status = f.status;
    if (f.gateway.trim() && f.gateway.trim() !== (p.gateway ?? '')) body.gateway = f.gateway.trim();
    if (!sameList(dns, p.dns_servers)) body.dns_servers = dns;
    if (Object.keys(body).length === 0) {
      open.value = false;
      return;
    }
    ok = !!(await action.run(() => networkApi.updatePool(p.id, body, { ifMatch: revisionTag(p.revision) }), {
      onConflict: () => emit('saved'),
      success: '地址池已更新',
    }));
  } else {
    if (!f.zone_id) return;
    const zoneId = f.zone_id;
    ok = !!(await action.run(
      (key) =>
        networkApi.createPool(
          {
            cidr: f.cidr.trim(),
            dns_servers: dns,
            gateway: f.gateway.trim(),
            name: f.name.trim(),
            range_end: f.range_end.trim(),
            range_start: f.range_start.trim(),
            zone_id: zoneId,
          },
          { key },
        ),
      { success: '地址池已登记' },
    ));
  }
  if (!ok) return;
  open.value = false;
  emit('saved');
}
</script>

<template>
  <Drawer v-model:open="open" destroy-on-close :title="editing ? `编辑地址池 ${pool?.name ?? ''}` : '登记地址池'" :width="560">
    <Alert class="mb-4" message="只登记，不修改宿主机网络。" show-icon type="info" />
    <Form ref="formRef" layout="vertical" :model="form">
      <FormItem label="名称" name="name" :rules="rules.name">
        <Input v-model:value="form.name" :maxlength="128" />
      </FormItem>
      <template v-if="!editing">
        <FormItem label="可用区" name="zone_id" :rules="rules.zone_id">
          <Select
            v-model:value="form.zone_id"
            :options="zones.filter((z) => z.status === 'active').map((z) => ({ label: `${z.name}（${z.code}）`, value: z.id }))"
          />
        </FormItem>
        <FormItem extra="规范网段，不能与本可用区已有地址池重叠" label="网段 CIDR" name="cidr" :rules="rules.cidr">
          <Input v-model:value="form.cidr" class="font-mono" placeholder="192.168.10.0/24" />
        </FormItem>
        <Row :gutter="12">
          <Col :span="12">
            <FormItem label="分配起始地址" name="range_start" :rules="rules.range_start">
              <Input v-model:value="form.range_start" class="font-mono" placeholder="192.168.10.100" />
            </FormItem>
          </Col>
          <Col :span="12">
            <FormItem label="分配结束地址" name="range_end" :rules="rules.range_end">
              <Input v-model:value="form.range_end" class="font-mono" placeholder="192.168.10.199" />
            </FormItem>
          </Col>
        </Row>
      </template>
      <template v-else>
        <div class="text-muted-foreground mb-3 font-mono text-xs">
          {{ pool?.cidr }} · {{ pool?.range_start }} – {{ pool?.range_end }}（网段与分配范围不可修改）
        </div>
        <FormItem label="状态" name="status">
          <Select
            v-model:value="form.status"
            :options="[
              { label: '启用', value: 'active' },
              { label: '停用', value: 'disabled' },
            ]"
          />
        </FormItem>
      </template>
      <FormItem
        :extra="editing ? '已登记网关不能清空' : '可选；须在网段内、分配范围外'"
        label="网关"
        name="gateway"
        :rules="rules.gateway"
      >
        <Input v-model:value="form.gateway" class="font-mono" placeholder="192.168.10.1" />
      </FormItem>
      <FormItem extra="回车或逗号分隔，最多 16 个，地址族与网段一致" label="DNS" name="dns_servers" :rules="rules.dns_servers">
        <Select v-model:value="form.dns_servers" class="font-mono" mode="tags" :open="false" :token-separators="[',', ' ']" />
      </FormItem>
    </Form>
    <Alert
      v-if="semanticsChanged"
      message="修改网关或 DNS 会停用该地址池的全部节点绑定并要求重新验证；已有地址被分配时后端拒绝修改。"
      show-icon
      type="warning"
    />
    <template #footer>
      <div class="flex justify-end gap-2">
        <Button @click="open = false">取消</Button>
        <Button :loading="action.pending.value" type="primary" @click="submit">{{ editing ? '保存' : '登记' }}</Button>
      </div>
    </template>
  </Drawer>
</template>
