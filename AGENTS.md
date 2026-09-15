# workbuddy2api 项目规范

本文件是项目级 AI 预设。全局规则（`~/.pi/agent/AGENTS.md`）继续适用，本文件在其之上补充
本项目特有的约束；两者冲突时以本文件为准。

## 一、测试与试运行：绝不干扰正在提供服务的旧进程

**背景**：本机上正在运行的旧实例（GUI 或 `wb2api.exe`）通常是上层客户端（如 CodeBuddy /
对话工具）正在使用的 token 服务。杀掉或重启它会让用户的对话立即中断。因此**任何**测试、
验证、试运行都必须在**独立并存**的实例上进行。

### 硬性要求

1. **绝不终止、重启、覆盖正在运行的旧进程/旧二进制。**
   - 不要 `taskkill` 打到旧 exe；不要覆盖旧 exe 文件本身（Windows 下覆盖运行中的 exe 会失败，
     更危险的是"看起来成功了"但实际让旧进程状态不一致）。
   - 需要停服务时，先确认 PID 与可执行文件路径属于**本次测试产物**，再停。

2. **测试产物必须使用新文件名。**
   - 不要用 `wb2api-gui.exe` 这类生产名做测试产物，改用带后缀的新名字，例如
     `wb2api-gui-test.exe`、`wb2api-gui-<特征>.exe`。
   - 构建命令示例：`go build -o wb2api-gui-test.exe ./cmd/gui`（GUI 需 comctl32 清单，
     见 `build-gui.ps1`；测试产物同样需要 `.syso`，它随包目录一起被链接，无需额外处理）。

3. **测试实例必须使用新端口 + 独立数据目录。**
   - 端口：不要复用生产 `listen` 端口（本机现为 `127.0.0.1:8787`）。改用一个确定且不冲突的端口
     （如 `8801`、`8802`…），并在启动前用 `netstat -ano | grep <port>` 确认空闲。
   - 目录：不要复用生产 `config.json` / `auths/` / `data/`。为每次测试建立独立目录
     （参考已有的 `credit-demo/`、`run-v2/` 做法），在其中放自己的 `config.json`、
     `data/`，`auth_dir` 可指向 `../auths` 只读复用凭证。
   - 这样新旧实例**并存**：旧实例继续服务，新实例可自由启停、重启、改配置。

4. **验证新实例时以它自己的端口为准。** 例如 `curl http://127.0.0.1:8801/healthz`，
   不要误打到旧端口。发生"服务在监听却返回 404/异常"时，先 `netstat -ano` 确认端口归属。

5. **收尾**：测试完成后只关闭本次启动的测试实例（按 PID + exe 路径双重确认），
   并清理测试产物；不要顺手停掉旧实例。

## 二、额度保护：验证一律离线，禁止消耗积分

**背景（真实事故）**：本项目账号多为一次性登录（手机号一次性、失效后无法二次登录），
积分是**不可再生资源**。此前曾有实现者为验证「账号优先级是否让选中概率变大」，在隔离
实例上连发 20 个真实 `/v1/chat/completions` 请求做分布统计——而该结论用单元测试
（`SetRandomSource` 注入确定性随机源 + 3000 次采样）即可精确验证。这既消耗了积分，
也违反了「能离线验证就不要打真实上游」的要求。

### 硬性要求

1. **验证的默认且唯一方式是离线。禁止在测试、验证、自检、调试中发起任何真实模型调用。**
   - 「真实模型调用」指任何会走到上游并被计费的请求，包括 `/v1/chat/completions`
     （流式与非流式）、以及任何间接触发上游 chat 的路径。
   - **不消耗积分**、可以自由使用的操作：`/healthz`、`/status`、读 `gui.log` 与
     `data/*.json` / `data/credit-log.jsonl`、读 `auths/*.json` 的元信息
     （`expiresAt` / `uid` / `nickname`）——但**不得**打印 token 明文。
   - **`/v1/models` 不是纯本地操作**：缓存（1h）命中时只读内存，但缓存过期或为空时
     会调 `FetchModels` **真实打上游**（`handler.go` 的 `fetchDynamicModels`）。
     验证期间为保证不产生任何上游流量，**不要调它**（需要模型清单就读 `gui.log` 里的
     `模型参数已加载：N 个模型` 或 `data/state.json`）。

2. **禁止以「更真实」为由打真实请求。** 以下均属违规（已发生或极易发生）：
   - 用真实请求统计选号分布、验证权重 / 优先级 / 冷却是否生效；
   - 启动 GUI 后发请求确认界面功能「真的能用」；
   - 验证路由 / 轮换 / 限流 / 重试等链路是否「实际工作」；
   - 「顺手跑一下看看有没有报错」。
   以上全部有离线替代（见下条）。

3. **优先使用的离线替代手段（本项目已有基建，直接复用，不要另造）：**
   - 选号 / 权重 / 优先级 / 分布类 → `pool` 包单元测试 + `p.SetRandomSource(fn)` 注入
     确定性随机源（可精确指定抽签结果，比真实随机更可测）；
   - 上游交互 / 错误处置 / 日志格式类 → `httptest` + `newFakeUpstream` /
     `newCountingFakeUpstream`（在 `internal/server/*_test.go`，可伪造任意状态码与报文）；
   - GUI 装配类 → 只验证「进程能启动、端口在监听、配置被正确读取、界面无 panic」；
     交互逻辑抽成纯函数做单元测试（参考 `cmd/gui/creditsum.go`、`probepage.go` 的写法）；
   - 端到端行为 → 读运行日志与落盘文件推断（`gui.log`、`state.json`、`credit-log.jsonl`
     已含足够证据）。

   > **注意启动本身也是有上游流量的**：GUI 启动时会做启动补签与一次模型参数拉取
   > （`loadModelRates` → `FetchModels`），首次额度刷新（`credit_refresh`）也会在
   > 间隔到达时请求 billing 接口。这些**不是 chat 调用、不消耗积分**，但确实会打上游。
   > 因此隔离实例应**尽快启用完就停**，不要长时间挂着。

4. **仅在用户明确要求「打一次真实请求」时才可调用模型**，且须同时满足：
   - **只允许 `deepseek-v4.1-flash`**（倍率最低）。严禁 `glm-*`、`kimi-*`、`minimax-*`、
     `hy*`、`deepseek-v4-pro` 等任何其他模型做试探性调用；
   - 最小规模：单条消息、`max_tokens` 取最小、串行不并发；
   - 调用前先说明「这一次会消耗积分」，调用后报告实际消耗估算。
   - 注意：本条**不是**对第 1 条的豁免门槛——用 flash 打 20 次真实请求同样违规，
     违规与否取决于「是否必要」，而不是「用了哪个模型」。

5. **不得销毁验证证据。** 隔离实例的日志与 `data/`（尤其 `credit-log.jsonl`）在验证结束后
   **不要立即删除**——它们是核对「是否真的没有消耗积分 / 是否发生异常」的唯一依据。
   确需清理时，先在回复中记录关键结论与账目自检结果，再删除。

## 三、部署与 GUI 机制约定（改名替换 / 原地重启 / 列表签名闸门）

### 改名替换部署（换掉正在运行的 exe，但不终结进程）

**背景**：用户允许「替换应用本身」。Windows 下不能覆盖运行中的 exe（写入被拒），
但同卷内**改名允许**。确立的部署做法：

1. `mv wb2api-gui.exe wb2api-gui.prevN.exe`（N 递增：`.prev` / `.prev2` / `.prev3`…）
2. `cp <新构建> wb2api-gui.exe`
3. 旧进程继续运行、继续服务；下次启动吃新版。

**`.prevN.exe` 命名是 load-bearing 约定，不得换别的形式**：托盘「重新启动」的
`relaunchTarget` / `swappedOutExeName`（`cmd/gui/main.go`）靠解析 `X.prevN.exe`
找回原名上的新 exe——运行中进程的 `os.Executable()` 返回的是改名后的 `.prevN`
路径，直接拉它会起回旧版。改掉这个备份命名 = 原地重启悄悄退化。

- 替换后提醒用户重启（托盘 →「重新启动」或手动退再开）；**不要替用户终结进程**。
- `.prevN.exe` 既是回滚备份，也可能是「正在运行的映像」——删之前确认对应进程已退。

### 干净构建

- 出生产 exe 用 `git worktree add <tmp> HEAD` 从已提交代码构建，避免把工作区
  未完成的 WIP 带进二进制；用完 `git worktree remove --force`。
- 命令等价 `build-gui.ps1`：
  `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -H windowsgui" -o wb2api-gui.exe ./cmd/gui`
  （`rsrc_windows_amd64.syso` 已提交在包内，链接器自动带上 comctl32 清单）。

### 账号列表刷新机制（签名闸门 + 轻量重绘）

**背景（历史 bug）**：`refreshAccounts` 曾用 `tblSignature` 比对 `lastSig` 却从不
写回——字段恒为空串，每 1.5s tick 都整表 `PublishRowsReset`（滚动重置、选中清空、
闪白），即「列表一直在刷新」。已修复为两级路径：

- `acctSigChanged`：比对 + 写回一体。签名相同 → `updateItems`（换数据快照 +
  `PublishRowsChanged` / LVM_REDRAWITEMS，只重绘可见单元格，不动滚动与选中）；
  不同 → 整表 `Replace` + 滚动位恢复。
- **新加显示字段时的规则**：值变化必须触发整表重建的 → 加进 `tblSignature`；
  高频运行态（在途、冷却倒计时等每秒都在变的）→ 不进签名，靠 `updateItems`
  重绘自然更新。
- 复选框显示值来自 `Status.Active`（在签名里），勾选变化必然整表重建对齐。

### 复选框 = 活跃号池

- 权威在 pool：`inactive` 集（持久化在 `state.json` 的 `inactive_accounts`）；
  `pick` / `AvailableUIDs` / 半开探测 / 粘性会话全部跳过未勾选账号；
  签到、额度刷新等维护路径**不受**勾选影响。
- GUI 侧 `accountModel` 实现 `walk.ItemChecker`；勾选状态按 UID 存 `active` map，
  `Replace` 时按 `Status.Active` 重建（行序变化不乱跳）。
- 实时生效链：点击 → `SetChecked` → `onActiveChange` → `svc.SetAccountActive`
  → `pool.SetActive` → dirty → flusher 5s 落盘。
- 「全选 / 反选 + 号池 n/m」位于账号列表正上方一行右侧（用户指定的位置）。
  反选到空不弹确认（用户明确知道后果 = 网关无号可用 503，点「全选」即恢复）。

### 托盘「重新启动」（原地重开进程）

- `relaunchSelf`：拉起独立 powershell 守望者（`Wait-Process` 本 PID 退出 →
  `Start-Process` 新 exe），随后正常退出。等旧进程退出再启动，避免抢 listen 端口。
- 与托盘里「启动/停止服务」不同：那对网关生效，这个是整个程序退出重开。
