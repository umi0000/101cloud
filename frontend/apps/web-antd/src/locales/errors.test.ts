import { describe, expect, it } from 'vitest';

import { describeError, describeTaskError } from './errors';

const error = (details: unknown, code = 'initialization_cloud_init_degraded') => ({
  code, message: '内部日志：密码 secret-user-data', details,
});
describe('Pt error presentation', () => {
  it('rounds embedded and structured Pt values without modifying source details or count precision', () => {
    const details = { unit: 'pt', requested: '1.250000', available: '0.049999', action: '请补充 1.250000 Pt 后重试' };
    const result = describeError({
      code: 'insufficient_credit',
      message: '本次申请 1.250000 Pt；可用额度不足',
      details,
    });
    expect(result.text).toBe('本次申请 1.3 Pt；');
    expect(result.details).toContain('可用 0.0 pt');
    expect(result.details).toContain('请补充 1.3 Pt 后重试');
    expect(details.requested).toBe('1.250000');
    expect(details.available).toBe('0.049999');
    expect(describeError(error({ unit: 'count', requested: '1.250000' }, 'quota_exceeded')).details)
      .toContain('本次申请 1.250000');
  });
});


describe('initialization diagnostic public boundary', () => {
  it('maps fixed diagnostic fields without treating done as initialization success', () => {
    const result = describeTaskError({ status: 'failed', error_code: 'initialization_cloud_init_degraded',
      error: error({ stage: 'login.cloud-ready', exit_code: 2, reason: 'cloud_init_degraded', cloud_init_status: 'done' }),
    }, 'request-1')!;
    expect(result.text).toBe('cloud-init 已带警告完成');
    expect(result.stage).toBe('网络与 root 账号凭据检查');
    expect(result.details).toContain('原因：cloud-init 已完成，但报告可恢复警告');
    expect(result.details).toContain('cloud-init 状态：已完成');
    expect(result.details).toContain('退出码：2');
    expect(result.requestId).toBe('request-1');
    expect(JSON.stringify(result)).not.toContain('secret-user-data');
  });

  it('preserves historical host-key failures without making SSH installation a password prerequisite', () => {
    const result = describeError(error({ stage: 'login.cloud-ready', exit_code: 2,
      reason: 'ssh_host_key_generation_missing_tool', cloud_init_status: 'degraded',
      raw: 'sensitive output', action: '任意上游文本',
    }, 'initialization_tool_missing'));
    expect(result.details.some((line) => line.includes('缺少 ssh-keygen'))).toBe(true);
    expect(result.details.some((line) => line.includes('网络与 root 账号凭据配置不要求 SSH 服务'))).toBe(true);
    expect(result.details.join('')).not.toContain('安装 OpenSSH');
    expect(result.details).toContain('退出码：2');
    expect(JSON.stringify(result)).not.toContain('sensitive output');
    expect(JSON.stringify(result)).not.toContain('任意上游文本');
  });

  it('explains password/public-key verification separately from SSH readiness and never echoes credential data', () => {
    const result = describeError(error({ stage: 'login.cloud-ready', reason: 'root_password_not_ready',
      root_password: 'secret-user-data', hashed_passwd: '$6$secret-hash', shadow: '$6$secret-shadow',
      ssh_public_keys: ['ssh-ed25519 secret-public-key'], authorized_keys: 'secret-authorized-key',
    }, 'initialization_password_not_ready'));
    expect(result.code).toBe('initialization_password_not_ready');
    expect(result.text).toContain('root 账号密码');
    expect(result.text).toContain('所提供的 SSH 公钥');
    expect(result.details.some((line) => line.includes('所提供的 SSH 公钥未正确写入'))).toBe(true);
    expect(result.details.some((line) => line.includes('不表示 SSH 服务是否就绪'))).toBe(true);
    for (const secret of ['secret-user-data', '$6$secret-hash', '$6$secret-shadow', 'secret-public-key', 'secret-authorized-key']) {
      expect(JSON.stringify(result)).not.toContain(secret);
    }
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
    ['initialization_password_not_ready', 'root 账号密码或所提供的 SSH 公钥尚未就绪'],
    ['initialization_marker_missing', 'cloud-init 启动完成标记缺失'],
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
