-- 0003_step_approval: 步骤日志增加 awaiting / approved 两个状态
--
-- 待审批的动作要落进步骤日志，批准后按原参数执行。见 kernel.requestApproval。

ALTER TABLE task_steps DROP CONSTRAINT IF EXISTS task_steps_status_valid;

ALTER TABLE task_steps ADD CONSTRAINT task_steps_status_valid CHECK (
    status IN ('pending', 'done', 'failed', 'skipped', 'awaiting', 'approved')
);
