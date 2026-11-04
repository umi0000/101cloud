import { describe, expect, it } from 'vitest';

import { describeError, describeTaskError } from './errors';

const error = (details: unknown, code = 'initialization_cloud_init_degraded') => ({
  code, message: '内部日志：密码 secret-user-data', details,
});

describe('initialization diagnostic public boundary', () => {
  it('maps fixed diagnostic fields without treating done as initialization success', () => {
    const result = describeTaskError({ status: 'failed', error_code: 'initialization_cloud_init_degraded',
      error: error({ stage: 'login.cloud-ready', exit_code: 2, reason: 'cloud_init_degraded', cloud_init_status: 'done' }),
    }, 'request-1')!;
    expect(result.text).toBe('cloud-init 已带警告完成');
    expect(result.stage).toBe('登录配置初始化检查');
    expect(result.details).toContain('原因：cloud-init 已完成，但报告可恢复警告');
    expect(result.details).toContain('cloud-init 状态：已完成');
    expect(result.details).toContain('退出码：2');
    expect(result.requestId).toBe('request-1');
    expect(JSON.stringify(result)).not.toContain('secret-user-data');
  });

  it('explains confirmed host-key generation tool failure without raw diagnostics', () => {
    const result = describeError(error({ stage: 'login.cloud-ready', exit_code: 2,
      reason: 'ssh_host_key_generation_missing_tool', cloud_init_status: 'degraded',
      raw: 'sensitive output', action: '任意上游文本',
    }, 'initialization_tool_missing'));
    expect(result.details).toContain('原因：SSH 主机密钥生成失败：缺少 ssh-keygen，请检查并安装 OpenSSH');
    expect(result.details).toContain('退出码：2');
    expect(JSON.stringify(result)).not.toContain('sensitive output');
    expect(JSON.stringify(result)).not.toContain('任意上游文本');
  });

  it.each(['secret-user-data', '__proto__', 'constructor', 'toString'])('does not print unrecognized values: %s', (value) => {
    const result = describeError(error({ reason: value, stage: value, cloud_init_status: value,
      action: '密码 secret-user-data', raw: value, user_data: value, field: value }));
    expect(result.stage).toBe('');
    expect(result.details).toEqual(['请联系管理员检查镜像依赖与初始化配置，并核对实例实际状态；不会自动重试。']);
  });

  it.each(['2', 1.5, null, {}, Number.POSITIVE_INFINITY])('rejects noninteger exit code %s without losing safe reason', (exit_code) => {
    const result = describeError(error({ exit_code, reason: 'required_tool_missing' }, 'initialization_tool_missing'));
    expect(result.details).toContain('原因：缺少初始化必需工具');
    expect(result.details.some((line) => line.startsWith('退出码：'))).toBe(false);
  });

  it.each([
    ['initialization_cloud_init_failed', 'cloud-init 初始化失败'],
    ['initialization_tool_missing', '初始化所需工具缺失'],
    ['initialization_marker_missing', '初始化完成标记缺失'],
    ['initialization_script_failed', '初始化脚本执行失败'],
    ['initialization_timeout', '等待初始化结果超时'],
    ['initialization_result_unknown', '初始化结果尚未确认'],
    ['initialization_diagnostic_unavailable', '初始化诊断暂不可用'],
  ])('supports fixed error code %s', (code, text) => {
    expect(describeError(error(null, code)).text).toContain(text);
  });

  it('does not equate exit 137 with a confirmed timeout', () => {
    const result = describeError(error({ reason: 'command_terminated', exit_code: 137 }, 'initialization_script_failed'));
    expect(result.text).toBe('初始化脚本执行失败');
    expect(result.details).toContain('退出码：137');
    expect(result.details).toContain('原因：初始化命令被终止，请核对是否存在超时或资源限制等原因');
    expect(describeError(error({ reason: 'template_timeout_invalid' }, 'initialization_script_failed')).details)
      .toContain('原因：初始化模板超时配置无效');
  });

  it('keeps unknown result and no-replay wording for task and step fallbacks', () => {
    expect(describeTaskError({ status: 'unknown', error_code: null })?.text).toContain('尚未确认');
    const result = describeTaskError({ status: 'unknown', error_code: 'initialization_result_unknown', error: error({ reason: 'result_unknown', stage: 'init.template' }, 'initialization_result_unknown') })!;
    expect(result.stage).toBe('初始化模板执行');
    expect(result.text + result.details.join('')).toContain('不会自动重新执行');
  });
});
