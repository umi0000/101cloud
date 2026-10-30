<script lang="ts" setup>
import type { AllocatableForm } from './node-forms';

import { Col, FormItem, InputNumber, Row } from 'ant-design-vue';

import { allocatableRules } from './node-forms';

/** 可分配量字段组；放在 Form 内，prefix 为该对象在表单 model 中的路径。 */
const props = withDefaults(defineProps<{ prefix?: string[] }>(), { prefix: () => [] });
const model = defineModel<AllocatableForm>({ required: true });
const rules = allocatableRules(() => model.value);
const name = (k: keyof AllocatableForm) => [...props.prefix, k];
</script>

<template>
  <Row :gutter="12">
    <Col :span="8">
      <FormItem label="vCPU" :name="name('vcpus')" :rules="rules.vcpus">
        <InputNumber v-model:value="model.vcpus" class="w-full" :min="1" :precision="0" />
      </FormItem>
    </Col>
    <Col :span="8">
      <FormItem label="内存 (GiB)" :name="name('memory_gib')" :rules="rules.memory_gib">
        <InputNumber v-model:value="model.memory_gib" class="w-full" :min="0" />
      </FormItem>
    </Col>
    <Col :span="8">
      <FormItem label="磁盘 (GiB)" :name="name('disk_gib')" :rules="rules.disk_gib">
        <InputNumber v-model:value="model.disk_gib" class="w-full" :min="0" />
      </FormItem>
    </Col>
  </Row>
  <div class="text-muted-foreground mb-2 text-xs">常驻预算：免额度（纳管、自定义）实例可占用的上限，不能超过总量。</div>
  <Row :gutter="12">
    <Col :span="8">
      <FormItem label="常驻 vCPU" :name="name('resident_vcpus')" :rules="rules.resident_vcpus">
        <InputNumber v-model:value="model.resident_vcpus" class="w-full" :min="0" :precision="0" />
      </FormItem>
    </Col>
    <Col :span="8">
      <FormItem label="常驻内存 (GiB)" :name="name('resident_memory_gib')" :rules="rules.resident_memory_gib">
        <InputNumber v-model:value="model.resident_memory_gib" class="w-full" :min="0" />
      </FormItem>
    </Col>
    <Col :span="8">
      <FormItem label="常驻磁盘 (GiB)" :name="name('resident_disk_gib')" :rules="rules.resident_disk_gib">
        <InputNumber v-model:value="model.resident_disk_gib" class="w-full" :min="0" />
      </FormItem>
    </Col>
  </Row>
</template>
