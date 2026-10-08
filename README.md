# AtomHelper 原子助手

7×24 常驻云端、事件驱动、带持久化记忆的个人智能体运行时。用户自部署在自己的服务器上。

## 设计文档

| 文档 | 内容 |
|---|---|
| [架构方案](docs/架构方案.md) | 总体架构、任务内核、记忆系统、技术选型 |
| [安全模型](docs/安全模型.md) | 信任等级、沙箱、数据出口管控、提示注入防护 |
| [协作与联邦](docs/协作与联邦.md) | 好友/团队、实例互联、生活工作分域 |
| [MCP 市场与 Skill](docs/MCP市场与Skill.md) | 工具分发、Skill 自装机制 |
| [截图理解与连接器](docs/截图理解与连接器.md) | 数据可达性、截图入口、审批阈值策略 |

## 当前进度：P0 内核 + P1 模型与工具 + P2 记忆系统

| 模块 | 内容 | 阶段 |
|---|---|---|
| `internal/core` | 任务控制块、状态机、步骤日志、信任等级、隐私分级、域与可见性 | P0 |
| `internal/store` | 持久化契约、内存实现、Postgres 实现、共享契约测试套件 | P0 |
| `internal/kernel` | 执行引擎 + 调度器：崩溃恢复、幂等重放、租约、预算硬上限、审批 | P0 |
| `internal/policy` | 隐私分级、数据出口管控、出站白名单、审批策略与累计帽子 | P1 |
| `internal/llm` | provider 抽象、按用途路由、**带信任围栏的上下文装配** | P1 |
| `internal/mcpgw` | MCP 网关：工具目录、出口管控、幂等透传、字段级信任判定 | P1 |
| `internal/memory` | 分层记忆、冲突仲裁与版本链、时效管理、四环渐进式披露 | P2 |
| `internal/agent` | 把内核、模型、工具、策略、记忆接起来 | P1/P2 |
| `internal/config` | 环境变量 + YAML 配置，非法值直接报错不静默回退 | P0/P1 |
| `cmd/atomd` `cmd/atomctl` | 常驻服务与运维 CLI | P0 |

| `internal/event` | 事件、订阅、订阅表达式的解析与求值（尚未接入调度器） | P3 |

**尚未开始**：事件的持久化与分发（P3 余下部分）、连接器（P4）、App（P5）。

### 记忆系统的几条规则

- **偏好必须条件化**：`{场景:个人通勤}→顺风车` 与 `{场景:接待客户}→豪华型` 是两条并存的
  记忆，不是互相冲突的一条。检索时按 特异度 > 置信度 > 新近性 排序。
- **写入时必须判定时效**，没有"默认永久"。限时记忆缺少绝对过期时间会被应用层和
  数据库约束双重拒绝。
- **安全属性走旁路强制注入**：过敏、忌口这类信息不参与相似度打分也不参与预算裁剪，
  相似度召回失败一次就是事故。它们也永不被遗忘机制归档。
- **冲突不默默覆盖**：先做交叉验证，再按冲突类型分级处置。用户执意覆盖时旧值转入
  版本链而非删除。

### 三条写进代码的硬约束

1. **绝密数据(L3)不传给任何工具**——含 L3 参数的动作直接拒绝，连审批机会都不给。
   给了审批机会，用户点一下确认就泄露了。
2. **untrusted 内容驱动的写操作一律手动确认**——由截图、邮件、网页推导出的动作，
   金额再小也不适用任何自动放行。
3. **工具输出不能具备指令资格**——配置里把 `trust` 写成 `user` / `system` 会被拒绝；
   且无论来源多可信，**自由文本一律降级为 external**。
4. **不可信来源写不进长期记忆**——截图、邮件、网页里的"事实"只能挂为候选，
   等用户在 App 上确认。

## 运行

```bash
docker compose up -d

export ATOM_DSN='postgres://postgres:atom@127.0.0.1:55432/atomhelper'
go run ./cmd/atomd -migrate-only     # 建表
go run ./cmd/atomd                   # 启动调度器（demo 模式）

# 另开一个终端
go run ./cmd/atomctl create "演示任务" "跑通内核"
go run ./cmd/atomctl show <任务ID>
go run ./cmd/atomctl steps <任务ID>

# 任务停在"等待用户确认"时
go run ./cmd/atomctl approve <任务ID>          # 按原参数执行
go run ./cmd/atomctl reject <任务ID> "理由"     # 不执行，交回规划器
# 任务因不可逆操作执行中断而挂起时
go run ./cmd/atomctl resolve <任务ID> done|failed
```

demo 流程依次经过：一次推理 → 一次只读查询 → 等待 10 秒 → 一次不可逆操作 → 完成，
用来观察 `waiting → ready` 唤醒和审批路径。**不会产生任何真实副作用。**

### 接入真实模型与工具

```bash
cp config.example.yaml config.yaml   # 按注释填写
export DEEPSEEK_API_KEY=sk-xxx       # 密钥只走环境变量，不写进配置文件
export ATOM_CONFIG=./config.yaml

go run ./cmd/atomd -check            # 只校验配置与连接
go run ./cmd/atomd
```

配置校验先于连接数据库执行，配置写错会立刻报错而不是跑起来之后出问题。

### 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `ATOM_DSN` | 必填 | PostgreSQL 连接串 |
| `ATOM_OWNER` | `atomd@主机名` | 写入执行租约，重启后应保持稳定 |
| `ATOM_WORKERS` | 4 | 并发执行任务数 |
| `ATOM_POLL_INTERVAL` | `1s` | 空闲轮询间隔 |
| `ATOM_LEASE` | `2m` | 执行租约时长，须显著大于轮询间隔 |
| `ATOM_MAX_STEPS_PER_CLAIM` | 16 | 单次领取内连续推进的步数上限 |
| `ATOM_LOG_LEVEL` | `info` | debug / info / warn / error |
| `ATOM_CONFIG` | 空 | YAML 配置路径。不设置则跑 demo 模式 |

## 测试

```bash
go test ./...
```

Postgres 契约测试默认跳过，需要真实数据库：

```bash
docker compose up -d
ATOM_TEST_DSN='postgres://postgres:atom@127.0.0.1:55432/atomhelper' go test ./internal/store/pg/
```

注意该测试会清空目标库，**不要指向生产数据库**。

## 开发

Go 1.24+。两个依赖因为上游要求 Go 1.25 而被钉住版本：

- `github.com/jackc/pgx/v5` → v5.7.5
- `github.com/modelcontextprotocol/go-sdk` → v1.0.0

## License

MIT
