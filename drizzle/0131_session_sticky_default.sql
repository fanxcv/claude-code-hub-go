-- 日期: 2026-09-21
-- 功能: 粘性改为会话级——反转 affinity_ignore_client_session_id 默认值
--
-- 为什么：该列原本默认 true（忽略客户端会话 ID，强制用前缀指纹粘性），那是前缀时代的
-- 默认。粘性改会话级后，默认必须是 false（不忽略 = 用会话绑定），否则新装部署一上线
-- 就是旧行为，改了代码等于没改。
--
-- 对既有行不做修改：保持用户当前配置（设计稿 §5 明确「不改存量」）。
-- 回滚：ALTER TABLE system_settings ALTER COLUMN affinity_ignore_client_session_id SET DEFAULT true;
ALTER TABLE "system_settings"
  ALTER COLUMN "affinity_ignore_client_session_id" SET DEFAULT false;