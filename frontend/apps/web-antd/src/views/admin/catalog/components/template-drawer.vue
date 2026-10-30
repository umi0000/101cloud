<script lang="ts" setup>
import type { InitTemplate, TemplatePhase } from '#/api/types';

import { computed, ref, watch } from 'vue';

import {
  Alert,
  Button,
  Divider,
  Drawer,
  Form,
  FormItem,
  Input,
  InputNumber,
  Radio,
  RadioGroup,
  Space,
  Table,
  Textarea,
} from 'ant-design-vue';

import { catalogApi } from '#/api/backend';
import { errorToast, useAction, useResource } from '#/composables/api';

import {
  CODE_PATTERN,
  DEFAULT_SCHEMA,
  parseSchema,
  SCRIPT_MAX_BYTES,
  scriptForbidden,
  utf8Bytes,
} from './rules';

type Mode = 'create' | 'edit' | 'view';

interface TemplateForm {
  code: string;
  name: string;
  phase: TemplatePhase;
  schema: string;
  script: string;
  timeout_seconds: number | undefined;
  version: string;
}

const props = defineProps<{ mode: Mode; source: InitTemplate | null }>();
const open = defineModel<boolean>('open', { default: false });
const emit = defineEmits<{ saved: [] }>();

/** 与后端 workerEnvName 一致：只有符合该规则的参数名才导出为 INIT_PARAM_<key>。 */
const ENV_NAME = /^[A-Z_a-z]\w*$/;

const SCRIPT_TEMPLATE = `#!/bin/sh
set -eu
# 初始化参数：INIT_PARAMETERS_JSON（完整 JSON），INIT_PARAM_<参数名>（单个值）
echo "init parameters: $INIT_PARAMETERS_JSON"
`;

function emptyForm(): TemplateForm {
  return {
    code: '',
    name: '',
    phase: 'post_start',
    schema: DEFAULT_SCHEMA,
    script: SCRIPT_TEMPLATE,
    timeout_seconds: 600,
    version: '',
  };
}

function formFrom(t: InitTemplate, copy: boolean): TemplateForm {
  return {
    code: t.code,
    name: t.name,
    phase: t.phase,
    schema: JSON.stringify(t.parameter_schema, null, 2),
    script: t.script,
    timeout_seconds: t.timeout_seconds,
    version: copy ? '' : t.version,
  };
}

const form = ref<TemplateForm>(emptyForm());
const tried = ref(false);
const action = useAction();
const detail = useResource(() => (props.source ? catalogApi.template(props.source.id) : null));
const readonly = computed(() => props.mode === 'view');

const title = computed(() => {
  if (props.mode === 'create') return props.source ? `基于 ${props.source.code} 新建版本` : '新建初始化模板';
  const t = detail.data.value ?? props.source;
  return `${props.mode === 'edit' ? '编辑草稿' : '初始化模板'} ${t?.code ?? ''} v${t?.version ?? ''}`;
});

async function fill() {
  tried.value = false;
  if (props.mode === 'create') {
    form.value = props.source ? formFrom(props.source, true) : emptyForm();
    return;
  }
  try {
    await detail.reload();
    if (detail.data.value) form.value = formFrom(detail.data.value, false);
  } catch (error) {
    errorToast(error);
  }
}

watch(open, (v) => {
  if (v) void fill();
});
watch(form, () => action.resetKey(), { deep: true });

const scriptBytes = computed(() => utf8Bytes(form.value.script));
const forbidden = computed(() => scriptForbidden(form.value.script));
const schemaResult = computed(() => parseSchema(form.value.schema));

interface EnvRow {
  description: string;
  exported: boolean;
  name: string;
  type: string;
}

const envRows = computed<EnvRow[]>(() => {
  const rows: EnvRow[] = [
    { description: '全部初始化参数的 JSON 对象', exported: true, name: 'INIT_PARAMETERS_JSON', type: 'object' },
  ];
  const r = schemaResult.value;
  if (!('value' in r)) return rows;
  const props_ = r.value.properties as Record<string, Record<string, unknown>>;
  for (const [key, s] of Object.entries(props_)) {
    const type = String(s.type ?? '');
    const valueText = type === 'object' || type === 'array' ? '值的 JSON 文本' : '值的文本形式';
    rows.push({
      description: ENV_NAME.test(key) ? `${String(s.title ?? s.description ?? key)}（${valueText}）` : '参数名不是合法环境变量名，只能从 INIT_PARAMETERS_JSON 读取',
      exported: ENV_NAME.test(key),
      name: `INIT_PARAM_${key}`,
      type,
    });
  }
  return rows;
});

const problems = computed(() => {
  const f = form.value;
  const out: string[] = [];
  if (props.mode === 'create' && (!CODE_PATTERN.test(f.code) || !CODE_PATTERN.test(f.version))) {
    out.push('代码与版本须为字母或数字开头，1–64 位字母、数字、_ . -');
  }
  if (!f.name.trim() || [...f.name].length > 128) out.push('名称为 1–128 个字符');
  if (!f.script) out.push('脚本不能为空');
  if (scriptBytes.value > SCRIPT_MAX_BYTES) out.push('脚本超过 256 KiB');
  if (forbidden.value.length > 0) out.push(`脚本不能包含：${forbidden.value.join('、')}`);
  const t = f.timeout_seconds;
  if (t === undefined || !Number.isInteger(t) || t < 1 || t > 86_400) out.push('超时须为 1–86400 秒的整数');
  const s = schemaResult.value;
  if ('error' in s) out.push(`参数 schema：${s.error}`);
  return out;
});

async function save() {
  tried.value = true;
  const s = schemaResult.value;
  const f = form.value;
  const timeout = f.timeout_seconds;
  if (problems.value.length > 0 || !('value' in s) || timeout === undefined) return;
  const body = {
    name: f.name,
    parameter_schema: s.value,
    phase: f.phase,
    script: f.script,
    timeout_seconds: timeout,
  };
  let r;
  if (props.mode === 'create') {
    r = await action.run((key) => catalogApi.createTemplate({ ...body, code: f.code, version: f.version }, { key }), {
      success: '已创建草稿',
    });
  } else {
    const sourceId = props.source?.id;
    if (!sourceId) return;
    r = await action.run(
      (key) => catalogApi.updateTemplate(sourceId, body, { ifMatch: detail.etag.value, key }),
      { onConflict: fill, success: '已保存草稿' },
    );
  }
  if (!r) return;
  open.value = false;
  emit('saved');
}

function formatSchema() {
  const s = schemaResult.value;
  if ('value' in s) form.value.schema = JSON.stringify(s.value, null, 2);
}
</script>

<template>
  <Drawer v-model:open="open" destroy-on-close :title="title" :width="860">
    <template v-if="!readonly" #extra>
      <Space>
        <Button @click="open = false">取消</Button>
        <Button :loading="action.pending.value" type="primary" @click="save">
          {{ mode === 'create' ? '创建草稿' : '保存草稿' }}
        </Button>
      </Space>
    </template>
    <Alert
      v-if="readonly"
      class="mb-3"
      message="已发布或已退役的模板不能修改。需要调整时点“新版本”，以此为底稿创建新的草稿版本。"
      show-icon
      type="info"
    />
    <Form :disabled="readonly" layout="vertical">
      <div class="grid grid-cols-3 gap-x-3">
        <FormItem label="代码" required>
          <Input v-model:value="form.code" class="font-mono" :disabled="mode !== 'create' || !!source" />
        </FormItem>
        <FormItem label="版本" required>
          <Input v-model:value="form.version" class="font-mono" :disabled="mode !== 'create'" />
        </FormItem>
        <FormItem label="名称" required><Input v-model:value="form.name" :maxlength="128" /></FormItem>
      </div>
      <FormItem label="执行阶段" required>
        <RadioGroup v-model:value="form.phase" class="flex flex-col gap-1">
          <Radio value="cloud_init">
            cloud_init：先在实例内执行 <code>cloud-init status --wait</code> 等待 cloud-init 完成，再执行脚本（镜像需包含 cloud-init）
          </Radio>
          <Radio value="post_start">post_start：实例启动后直接执行脚本</Radio>
        </RadioGroup>
      </FormItem>
      <FormItem label="超时" required extra="1–86400 秒。整个初始化（cloud_init 阶段含等待 cloud-init）共用这个时限，到时强制终止（SIGKILL）">
        <InputNumber v-model:value="form.timeout_seconds" :max="86400" :min="1" :precision="0" addon-after="秒" class="w-48" />
      </FormItem>

      <Divider orientation="left" plain>脚本</Divider>
      <div class="text-muted-foreground mb-2 text-xs leading-5">
        实例启动时在实例内以 <code>/bin/sh -c</code> 执行，PATH 为
        <code class="font-mono">/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin</code>。退出码 0
        视为成功；非 0 或超时会使交付任务进入“结果待核对”。同一实例对同一模板成功执行一次后不再重复执行。输出不会被记录。
      </div>
      <Alert
        v-if="forbidden.length > 0"
        class="mb-2"
        :message="`脚本包含后端拒绝的内容：${forbidden.join('、')}。不要在脚本中写入密码、密钥或令牌。`"
        show-icon
        type="error"
      />
      <Textarea
        v-model:value="form.script"
        :auto-size="{ minRows: 14, maxRows: 32 }"
        class="font-mono text-xs"
        :readonly="readonly"
        spellcheck="false"
        wrap="off"
      />
      <div class="mt-1 text-right text-xs" :class="scriptBytes > SCRIPT_MAX_BYTES ? 'text-red-500' : 'text-muted-foreground'">
        {{ scriptBytes.toLocaleString() }} / {{ SCRIPT_MAX_BYTES.toLocaleString() }} 字节
      </div>

      <Divider orientation="left" plain>参数 schema</Divider>
      <div class="text-muted-foreground mb-2 text-xs leading-5">
        JSON Schema 子集。允许的键：type、properties、required、additionalProperties、enum、minimum、maximum、minLength、maxLength、items、maxItems、description、title。
        根节点必须是 object；每个 object 都要有 properties，并显式写 <code>"additionalProperties": false</code>。不能包含密码、密钥、令牌、cloud-init 等字样。
        参数值由管理员在直接下发自定义实例时填写。成员端读不到 schema，标准套餐创建实例时参数固定为空对象，所以套餐引用的模板不应有 required 参数。
      </div>
      <Textarea
        v-model:value="form.schema"
        :auto-size="{ minRows: 6, maxRows: 24 }"
        class="font-mono text-xs"
        :readonly="readonly"
        spellcheck="false"
      />
      <div class="mt-1 flex items-center justify-between">
        <span v-if="'error' in schemaResult" class="text-xs text-red-500">{{ schemaResult.error }}</span>
        <span v-else class="text-xs text-green-600">schema 合法</span>
        <Button v-if="!readonly" size="small" :disabled="'error' in schemaResult" @click="formatSchema">格式化</Button>
      </div>
    </Form>

    <Divider orientation="left" plain>脚本可用的环境变量</Divider>
    <Table
      :columns="[
        { dataIndex: 'name', title: '变量' },
        { dataIndex: 'type', title: '类型', width: 90 },
        { dataIndex: 'description', title: '说明' },
      ]"
      :data-source="envRows"
      :pagination="false"
      row-key="name"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.dataIndex === 'name'">
          <span class="font-mono" :class="record.exported ? '' : 'text-muted-foreground line-through'">{{ record.name }}</span>
        </template>
      </template>
    </Table>
    <div class="text-muted-foreground mt-1 text-xs">字符串原样传入；数字、布尔转为文本；对象和数组为 JSON 文本。未填写的参数不会出现。</div>

    <Alert v-if="tried && problems.length > 0" class="mt-3" message="请修正以下问题" show-icon type="error">
      <template #description>
        <ul class="m-0 list-disc pl-4">
          <li v-for="p in problems" :key="p">{{ p }}</li>
        </ul>
      </template>
    </Alert>
  </Drawer>
</template>
