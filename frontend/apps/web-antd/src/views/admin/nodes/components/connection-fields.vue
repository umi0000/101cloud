<script lang="ts" setup>
import type { NodeConnectionInput } from '#/api/types';

import { FormItem, Input, InputPassword } from 'ant-design-vue';

import { connectionRules } from './node-forms';

/** 节点连接字段组；放在 Form 内，prefix 为该对象在表单 model 中的路径。 */
const props = withDefaults(defineProps<{ prefix?: string[] }>(), { prefix: () => [] });
const model = defineModel<NodeConnectionInput>({ required: true });
const name = (k: keyof NodeConnectionInput) => [...props.prefix, k];
</script>

<template>
  <FormItem
    extra="必须是 HTTPS 源，且在后端 CLOUD_NODE_ALLOW_CIDRS 允许的范围内"
    label="Agent 地址"
    :name="name('endpoint')"
    :rules="connectionRules.endpoint"
  >
    <Input v-model:value="model.endpoint" class="font-mono" placeholder="https://10.0.0.5:8443" />
  </FormItem>
  <FormItem
    extra="只写，提交后不再返回"
    label="Agent 令牌"
    :name="name('token')"
    :rules="connectionRules.token"
  >
    <InputPassword v-model:value="model.token" autocomplete="new-password" class="font-mono" />
  </FormItem>
  <FormItem
    extra="从 Agent 安装输出或其他可信渠道获取，不要使用探测结果"
    label="服务端证书 SHA-256"
    :name="name('server_cert_sha256')"
    :rules="connectionRules.server_cert_sha256"
  >
    <Input v-model:value="model.server_cert_sha256" class="font-mono" :maxlength="64" placeholder="64 位小写十六进制" />
  </FormItem>
</template>
