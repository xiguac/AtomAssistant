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

## 当前进度：P0 内核 + P1 模型与工具 + P2 记忆系统 + P3 事件驱动 + P4 日历与邮件

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
| `internal/event` | 事件总线：订阅表达式、命中即唤醒、任务收件箱 | P3 |
| `internal/schedule` | 定时任务：到点创建任务，停机不补跑，上一次没结束不叠加 | P3 |
| `internal/webhook` | 外部系统投递事件的 HTTP 入口，令牌鉴权，内容一律按外部对待 | P3 |
| `internal/vault` | 隔离保险库：敏感信息只进不出，模型与日志只见句柄 | — |
| `internal/pay` | 支付宝 Agent 支付：收银台付款、按次收费的资源（HTTP 402） | — |
| `internal/connector/calendar` | 日历连接器：CalDAV 直连，查日程、建、改、删 | P4 |
| `internal/connector/holiday` | 节假日与调休安排：读取公开的日历订阅 | P4 |
| `internal/connector/mail` | 邮件连接器：IMAP 读取与整理、SMTP 发信 | P4 |
| `internal/channel` | 消息渠道：聊天软件里的话变成任务，任务的进展推回去；QQ 机器人适配器 | — |
| `internal/style` | 学习用户本人的说话方式：攒样本、提炼概述 | — |
| `internal/skill` `internal/skillscan` | Skill 安装器、运行时工具集收窄、安装前扫描闸门 | P9（本地安装部分） |

**尚未开始**：截图理解、提醒事项与通讯录、连接器的变更推送（P4 余下部分）、App（P5）、MCP 与 Skill 市场（P8/P9 的分发部分）。

### 它怎么做事

- **综合研判，不看单一因素。** 问"十月去哪玩"，它不会只评价某个地方好不好玩，
  而是把出发地、可行日期、车票机票、天气、节假日、人流、你的出行习惯放在一起权衡，
  给出什么时候去、去哪、怎么去。哪一项没有数据，结论里会单独写明，不编造。
- **标准流程（SOP）。** 旅游规划、宴请订餐、购物决策这类事有内置流程；
  做过的事会被提炼成新流程，等你确认后生效，下次照着做。
- **越用越懂你。** 任务结束后自动记下你提到的住处、习惯、身边人的忌口等。
  只有能在你的原话里逐字找到依据的才直接记住；其余的（包括从邮件、网页里读到的）
  一律先挂着等你确认。
- **会问，但不乱问。** 缺的信息会改变结论、记忆里又没有时才问，一次问一个。
- **长程任务不迷路。** 分好几天收集材料这类事，它自己维护一份清单，
  你随时可以补充内容——哪怕它当时没在等你。等待的时间不计入预算。
- **只说必要的。** 调工具时只带这一次必需的信息，查天气给城市不给住址；
  也不会为了找线索去翻你的邮件。
- **回复风格可配。** 默认简洁，可在配置里改成适中或详细，或写上你自己的要求。

### 事件唤醒

任务可以等一个事件而不是等一段时间。规划器给出订阅表达式和兜底超时，
事件命中时任务被唤醒，事件内容作为一条步骤写进它的日志——信任等级沿用事件自身的，
一封邮件能把任务叫醒，但它的正文进了上下文依然是不可信材料。

```
topic == "mail.received" AND payload.from CONTAINS "张总"
```

字段只有 `topic`、`source`、`payload.路径`；运算符 `== != > >= < <= CONTAINS`，
用 `AND / OR / NOT` 和括号组合。不支持函数调用与正则。

### 记忆系统的几条规则

- **偏好必须条件化**：`{场景:个人通勤}→顺风车` 与 `{场景:接待客户}→豪华型` 是两条并存的
  记忆，不是互相冲突的一条。检索时按 特异度 > 置信度 > 新近性 排序。
- **写入时必须判定时效**，没有"默认永久"。限时记忆缺少绝对过期时间会被应用层和
  数据库约束双重拒绝。
- **安全属性走旁路强制注入**：过敏、忌口这类信息不参与相似度打分也不参与预算裁剪，
  相似度召回失败一次就是事故。它们也永不被遗忘机制归档。
- **冲突不默默覆盖**：先做交叉验证，再按冲突类型分级处置。用户执意覆盖时旧值转入
  版本链而非删除。

### 付款

助手可以用支付宝付款，接的是支付宝的 [Agent 支付](https://aipay.alipay.com/products/agent-pay)。
支付宝没有给智能体开放 HTTP 接口，付款方的能力都在它官方的命令行程序 `alipay-bot` 里，
这里把助手用得到的几个子命令包成了内置工具。

```bash
npx -y @alipay/agent-payment@latest install   # 安装官方的 alipay-bot（支付宝提供）
atomctl pay wallet open                        # 开通 AI 钱包，按提示在支付宝里授权
atomctl pay wallet status
```

然后在配置里打开：

```yaml
payment:
  alipay:
    enabled: true
    resource_domains: [api.example.com]   # 可选：允许助手请求的按次收费接口
```

两种付款场景：

- **收银台付款**：别的工具下单后返回了支付宝收银台链接，助手用 `pay.alipay_submit` 发起付款。
- **按次收费的资源**：助手请求一个接口，对方回 HTTP 402 要求付费，助手用 `pay.resource_pay` 付款后取回内容。
  只能请求你在 `resource_domains` 里逐个列出的域名——这是助手唯一能主动向外发请求的地方。

每一笔钱过两道关：

1. **你在这里确认**。发起付款是不可逆操作，默认每一笔都要 `atomctl approve`。
   收银台付款可以配"多少钱以下自动放行"；按次收费的资源金额无法由系统核实，永远要确认。
   由邮件、网页这类外部内容驱动的付款，无论金额多小都要确认。
2. **你在支付宝里授权**。助手拿到的只是一个待确认订单和一个链接，它把链接原样交给你，
   你点开授权之后钱才付出去。

钱包的开通、绑定、关闭只能用 `atomctl pay wallet` 操作，不是助手的工具。
按 Skill 执行的任务，只有 Skill 明确声明了付款工具才能付款。

### 在聊天软件里和它说话

不用开终端：接上消息渠道之后，直接在 QQ 里给助手发消息。

- 你发一句话，就是交代一件事。助手问了问题，你接着回就是回答。
- 有动作等你确认时，它会把要做的事完整发给你（一封信会带上收件人和正文）。
  回「同意」「好」「行」就执行；回别的任何话都当作修改意见，它照着改完再给你看。
- 办完了、办不成，都会主动告诉你。同一个进展只说一次，重启也不会重复。
- 想另起一件不相干的事而前一件还在等你，用 `/new 要办的事`。

目前支持 QQ 机器人（单聊）。接入步骤：

```bash
# 1. 在 QQ 开放平台创建机器人，拿到 AppID 和 AppSecret；把 AppSecret 存进保险库
atomctl vault put qqbot "QQ 机器人 AppSecret" channel.qq

# 2. 在配置里写 channels 一节（见 config.example.yaml），owners 先留空，启动 atomd

# 3. 在 QQ 里给机器人发一句话。它会回你一串标识——把它填进 owners，重启
```

**谁的话算数。** 渠道必须是你亲手写进配置文件的，助手没有任何办法自己开通渠道。
但一个机器人谁都能给它发消息，所以每个渠道还要列出主人（`owners`）：
只有主人发来的话会变成任务，并且带着你本人的权限。其他人发来的一律不处理，
只回一句"这是私人助手"，外加他自己的标识（你第一次接入时就靠这个知道该填什么）。

连接方式是我们主动连出去的 WebSocket，不需要公网地址，放在家里的机器或内网服务器上也能用。
飞书、钉钉沿用同一套接口，尚未实现。

### 日历与邮件

日历走 CalDAV，邮件走 IMAP/SMTP，都是开放协议，直连，不经过 MCP。
iCloud、Fastmail、Nextcloud、QQ 邮箱、163、企业邮箱都能接。

```bash
# 1. 密码放进保险库，授权给两个连接器。iCloud 要用「App 专用密码」，不是 Apple ID 的登录密码
atomctl vault put icloud "iCloud App 专用密码" connector.calendar,connector.mail

# 2. 在配置里写 connectors 一节（见 config.example.yaml），密码一栏只能写 vault://icloud

# 3. 试连，确认账号、密码、服务器地址都对
atomctl connector check
```

接上之后助手多了这些工具：

| 工具 | 做什么 | 默认 |
|---|---|---|
| `calendar.list` `calendar.events` | 列日历、查某段时间的日程（重复日程逐次展开） | 直接执行 |
| `calendar.create` `calendar.update` | 新建、修改日程；重复日程可以只改或取消某一次 | 直接执行（见下） |
| `calendar.delete` | 删除日程 | 要你确认 |
| `calendar.holidays` | 查法定节假日与调休安排（放假、补班） | 直接执行 |
| `mail.folders` `mail.search` `mail.read` | 列文件夹、搜索、读正文（长信分段） | 直接执行 |
| `mail.mark` `mail.move` | 标已读/星标、移动（归档、移进废纸篓） | 要你确认 |
| `mail.send` | 发信、回复 | 要你确认 |

**日历的新建和修改，按"这件事是谁说的"来定要不要确认：**

- **你亲口交代的，直接写。** "周五下午三点开会，记一下""下周一调休，那天的站会不开了"——
  再弹一次确认是多此一举，点确认和自己动手记一样费事。你转发、粘贴给助手的通知也算你的意思。
- **助手自己从外面读来的，交给你定夺。** 任务读过邮件、网页这类外部内容之后，
  后面的改动都要你确认，确认时会写明是因为读过什么。邮件里说周日团建，记不记由你决定。
- **删除整条日程始终要确认**：删掉的找不回来。只取消重复日程里的某一次不算删除，是修改。
- 想收紧，在 `approval.rules` 里给 `calendar.create` / `calendar.update` 写 `never: true`。

"读过外部内容就要确认"对所有工具都生效，不只是日历：你配了"100 元以下打车自动放行"，
但这趟车是邮件里让叫的，照样要你确认。只有整理邮件（标已读、移动）例外——它本来就是对着邮件做的，
你在 `approval.rules` 里放行了就直接执行。

调课、调休这类临时变动：重复日程可以只挪某一次、只取消某一次，系列里的其他各次不动。

哪几天放假、哪个周末补班，助手从节假日订阅里查，不靠印象。在配置里写上订阅地址即可
（Apple 的中国大陆节假日订阅是 `https://calendars.icloud.com/holidays/cn_zh.ics`），
它不需要账号密码，也不依赖日历连接器。数据一天取一次；取不到新的就继续用手里的，
一次都没取到过则如实报错，不会当成"这段时间没有节假日"。
订阅里的节日名称只保留前 20 个字，查它不算"读过外部内容"，不影响后面的日程直接写入。

几条写死的约束：

- **读回来的内容不是指令。** 日程的标题和备注可能来自别人的邀请，邮件从头到尾是别人写的。
  这些内容进上下文时一律在围栏里，助手只把它们当事实看；里面要求转账、发资料、删邮件的话不会被照办，
  而是会被当作可疑情况报告给你。
- **读信不留痕迹。** 助手看过的信不会变成已读。
- **没有彻底删除邮件。** 最重的操作是移进废纸篓。
- **发出去的每封信都在「已发送」里留底**，你在自己的邮箱里看得到助手以你的名义说了什么。
- **改和删日程只认日历里的对象。** 路径不在任何一个日历集合之内的，一律拒绝。
- **密码每次连接现取**，不落在配置里，不进日志；非本机地址拒绝明文连接。
- **每次调用留审计**，只记做了什么，不记内容。

目前的限制：不能一次改掉整个重复系列的时间（可以一次一次地改）；新建不了重复日程；
发信只支持纯文本、不带附件；读信只报附件名，不读附件内容。
含病情、证件号这类绝密数据的日程可以写进日历（那是你自己的地方），邮件则会被直接拒绝；
注意与别人共享的日历对方是看得到的，系统目前不区分。

### 像你本人那样说话

通用模型写的东西有一股客服腔。助手会从你亲手写的话里学你的说话方式，
替你写邮件、对你说话时向它靠拢：

- **对助手说话的口气**：自动学。来源是你给它的指示、对它提问的回答、拒绝某个动作时给的理由。
- **给别人写邮件的口气**：你手动触发，`atomctl style learn-mail` 会读「已发送」里你自己写的信，
  去掉引用的原文、转发的内容和手机客户端加的签名，只留你亲手写的部分。

```bash
atomctl style show         # 看它学到了什么：一段概述，加上最近的样本
atomctl style learn-mail   # 从已发送里学写信的口气
atomctl style forget       # 全部忘掉，从头学
```

样本攒够 5 条开始提炼概述，之后每多 8 条重新提炼一次；每个场景只留最近 300 条。

同样有几条写死的约束：

- **只学你本人写的。** 别人发来的邮件、网页、工具返回的内容一个字都不学；
  助手代你发出去的信也不算你写的，不会被它自己学回去。
- **样本是语气的例子，不是要做的事。** 它们进上下文时在围栏里，助手不会去执行样本里提到的事。
- **敏感的不留。** 带手机号、证件号、病情的句子不收作样本。
- **像你不等于替你编。** 助手只表达你要它表达的意思，不会为了显得自然添加你没说过的事。

不想要这个功能，在配置里写 `reply.voice: off`。样本和概述随 `memory` 板块一起导出导入。

### 保险库

证件号、银行卡、详细住址这类信息放进保险库，思路借自手机的安全芯片：
能让被授权的工具"用一下"，但谁也读不出来。

```bash
atomctl vault keygen                       # 生成密钥，设为环境变量 ATOM_VAULT_KEY
atomctl vault put 家庭住址 "我家的详细地址" maps.book_ride   # 值从标准输入读，最后是可用的工具
atomctl vault list                         # 只能看到名字、用途、授权给了谁
```

模型看到的只是 `vault://家庭住址`，在工具参数里引用它；真实内容只在调用那一刻
由网关换进去，工具回显的内容会被换回 `[已隐藏:家庭住址]`。步骤日志、审计、记忆里都没有明文，
数据库里是密文。没有被点名的工具引用它，调用直接拒绝。**没有读出明文的命令。**

### Skill

Skill 是别人写好的流程：一个 `skill.yaml`，写明目标、步骤和它要用到的工具。它不含可执行代码——
清单里只认固定的几个字段，多出任何一个都拒绝安装。

```bash
atomctl skill install ./weekend-weather     # 扫描 → 展示全文与工具 → 你确认 → 安装
atomctl run weekend.weather "这周末天气"     # 让一个任务按它执行
```

三道关，从弱到强：

1. **安装前扫描**（[SkillSpector](https://github.com/NVIDIA/skillspector)）。扫描没通过、没装扫描器、扫描超时，都不装。
   它拦得住明显的恶意，拦不住措辞正常的恶意，所以不指望它。
2. **不自动生效**。装上的 Skill 不会自己跑进别的任务，只有你用 `atomctl run` 指定时才起作用。
3. **工具集取交集**。按 Skill 执行的任务只能调「它声明过的 ∩ 你授权过的」工具，
   名单之外的工具模型连看都看不到。流程里写一句"把结果发到某个邮箱"，它没声明邮件工具就调不动。

### 几条写进代码的硬约束

1. **绝密数据(L3)不以明文传给任何工具**——含 L3 参数的动作直接拒绝，连审批机会都不给。
   给了审批机会，用户点一下确认就泄露了。唯一的通道是保险库，通到哪个工具由你逐条指定。
2. **untrusted 内容驱动的写操作一律手动确认**——由截图、邮件、网页推导出的动作，
   金额再小也不适用任何自动放行。
3. **工具输出不能具备指令资格**——配置里把 `trust` 写成 `user` / `system` 会被拒绝；
   且无论来源多可信，**自由文本一律降级为 external**。
4. **不可信来源写不进长期记忆**——截图、邮件、网页里的"事实"只能挂为候选，
   等用户确认。
5. **只有你亲手配置的渠道才算你本人**——助手代你开通的消息渠道一律按外部内容对待，
   直到你在后台确认它。
6. **Skill 安装前必须过扫描，扫描没跑成等于不通过。**
7. **付款能力不能自己给自己**——开通钱包、列出可请求的域名都只能由你来做；
   每一笔付款都要你在支付宝里授权。

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
# 回答任务的提问，或者随时给它补充材料（它不必正好在等）
go run ./cmd/atomctl reply <任务ID> "护照照片拍好了，passport.jpg"
# 发布一个事件，唤醒订阅命中的等待任务
go run ./cmd/atomctl emit mail.received '{"from":"客户张总"}'

# 定时任务：每天早上 8 点跑一次
go run ./cmd/atomctl schedule add 24h 08:00 "早报" "看一眼今天的天气和日程，有要注意的告诉我"
go run ./cmd/atomctl list                 # 进行中的任务；list all 连已结束的一起看

# 记忆：查看、确认或否决助手提炼出来的东西
go run ./cmd/atomctl memory list
go run ./cmd/atomctl memory pending
go run ./cmd/atomctl memory confirm <ID>
```

### Webhook

让外部系统（邮件网关、智能家居、自己写的脚本）往事件总线里投递事件，唤醒在等它的任务：

```bash
export ATOM_WEBHOOK_ADDR=127.0.0.1:8787
export ATOM_WEBHOOK_TOKEN=<至少 24 个字符的随机串>

curl -X POST http://127.0.0.1:8787/events/mail.received   -H "Authorization: Bearer $ATOM_WEBHOOK_TOKEN"   --data-binary '{"from":"客户张总","body":"..."}'
```

从这里进来的内容一律是 external 等级，调用方无法自报更高的等级：它能把任务叫醒，
但内容只是材料不是指令，由它驱动的写操作必须你确认。没有令牌就不开门，没有免鉴权模式。
默认建议只监听回环地址，需要对外时放在反向代理和 TLS 后面。

### 迁移服务器

```bash
# 旧机器：按板块导出，或者一次全导
atomctl export memory memory.json
atomctl export all backup.json          # memory / tasks / events / vault / skills

# 新机器：先 atomd -migrate-only 建表，再导入
atomctl import backup.json
```

导出在同一个快照里完成，导入在一个事务里完成；已存在的数据不会被覆盖，同一个文件导两次是安全的。
新机器上的程序版本不能比导出时旧。保险库导出的是密文，**密钥 `ATOM_VAULT_KEY` 要自己带过去**；
`config.yaml` 和模型密钥同理，它们不在数据库里。

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
| `ATOM_VAULT_KEY` | 空 | 保险库密钥。不设置则保险库锁定：能列名字，不能存也不能用 |
| `ATOM_WEBHOOK_ADDR` | 空 | Webhook 监听地址。不设置则不开 |
| `ATOM_WEBHOOK_TOKEN` | 空 | Webhook 令牌，至少 24 个字符。开了入口就必须设置 |
| `ATOM_SKILL_SCANNER` | `skillspector` | Skill 安装前扫描器的可执行文件 |
| `ATOM_SKILL_SCANNER_ARGS` | 空 | 追加给扫描器的参数，按空白分隔 |

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

日历和邮件连接器的集成测试同样默认跳过，需要本地的测试服务器：

```bash
docker run -d -p 127.0.0.1:55232:5232 tomsquest/docker-radicale
docker run -d -p 127.0.0.1:55143:3143 -p 127.0.0.1:55025:3025 \
  -e GREENMAIL_OPTS="-Dgreenmail.setup.test.all -Dgreenmail.hostname=0.0.0.0 -Dgreenmail.auth.disabled" \
  greenmail/standalone:2.1.2

ATOM_TEST_CALDAV=http://127.0.0.1:55232 \
ATOM_TEST_IMAP=127.0.0.1:55143 ATOM_TEST_SMTP=127.0.0.1:55025 \
  go test ./internal/connector/...
```

## 开发

Go 1.24+。两个依赖因为上游要求 Go 1.25 而被钉住版本：

- `github.com/jackc/pgx/v5` → v5.7.5
- `github.com/modelcontextprotocol/go-sdk` → v1.0.0

## License

MIT
