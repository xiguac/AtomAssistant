-- 0005_memory_builtin_source: 记忆来源增加 builtin
--
-- 随程序发布的内置流程（SOP）既不是用户说的，也不是任务里提炼的。

ALTER TABLE memory_revisions DROP CONSTRAINT IF EXISTS memory_revisions_source_valid;

ALTER TABLE memory_revisions ADD CONSTRAINT memory_revisions_source_valid CHECK (
    source_kind IN ('user', 'task', 'connector', 'observed', 'builtin')
);
