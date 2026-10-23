# v0.2.18 上游同步记录

本次将上游标签 `v0.2.18`（`ac1b8f1271be0d674fa6f074403c1410f30a3a81`）合入动态额度维护分支，代码版本为 `0.2.18-ablett.1`。同步起点为已发布的 `v0.2.17-ablett.1`。本记录用于代码升级和部署准备，不代表已部署到生产。

## 合并处理

- 统一扣费命令保留用户动态额度的实际扣费、执行周期及预留信息，同时引入上游的账号日/周配额语句选择字段；这些可选元数据继续排除在既有幂等指纹之外。
- 保留动态额度管理页面、实时估值、每轮准入、跨组回退和 Fork 升级校验，接入上游 Vertex 配额和账号管理页面改进。
- 后端、容器构建和 CI 版本检查统一为上游要求的 Go 1.27.3，发布工作流继续执行维护版计费、管理组件和兼容性检查。
- 新增上游迁移 `246_enable_gemini_image_generation_groups.sql`，原有 `246_ablett_account_user_quota.sql` 保持字节不变。迁移记录以完整文件名及 checksum 标识，两份 `246` 迁移分别执行和核验。

## 升级路径与回退

从旧维护版结构升级时，新增的上游迁移会开启已有 Gemini / Antigravity 分组的生图权限；Composite 分组沿用现有设置。动态额度的历史已用、真实周期和未知占用继续保留。

从官方 `v0.2.18` 结构首次切换时，上游生图迁移已有记录，维护版只补充尚未应用的迁移，不重新覆盖管理员后续更改的生图权限。初始化承接当前周期的旧账单，并保留官方扣费幂等键，避免重放或重复初始化造成重复扣费和重复累计。

已应用 `v0.2.18` 迁移的数据库，应使用对应的 `0.2.18-ablett.1` 维护制品进行首次切换。`0.2.17-ablett.1` 的迁移清单不包含新增上游迁移，不能把它作为此次首次切换或回退的默认程序。回退仍须核验完整迁移清单与备份，必要时配套恢复数据库。

## 验证记录

Go 验证使用 Go 1.27.3，`GOMAXPROCS=2`，包并行度为 2；前端测试最多两个 Worker。

| 检查 | 结果 |
| --- | --- |
| `CI=1 make test-unit`，工作目录 `backend` | 完整后端单测通过 |
| `CI=1 make test-integration`，工作目录 `backend` | 完整集成测试通过，使用真实 PostgreSQL 18.1 与 Redis 8.4；包含两条隔离数据库升级路径及旧扣费幂等键重放 |
| `pnpm exec vitest run --maxWorkers=2 --minWorkers=1`，工作目录 `frontend` | 372 个文件、2879 项测试通过 |
| `pnpm run lint:check`、`pnpm run build`，工作目录 `frontend` | 静态检查、i18n、TypeScript 与前端构建通过；沿用已有分包大小提示 |
| 发布 helper 与安装兼容校验 | 分别 13 项、4 项通过 |
| `openspec validate add-user-account-dynamic-quota --type change --strict --no-interactive`、shell 语法与 `git diff --check` | 通过 |
| 迁移源文件比对 | 所有上游迁移与 `v0.2.18` 一致；自定义配额迁移与 `v0.2.17-ablett.1` 一致 |

以下定向并发检查通过，覆盖动态额度、共享模型映射、限流写时复制和调度快照缓存：

```bash
GOMAXPROCS=2 go test -p 2 -race -tags=unit ./internal/service ./internal/repository -run 'Test(AccountUserQuota|AccountPrepareSharedSnapshot|SetAccountModelRateLimitSnapshot|ApplyAccountSchedulingThreshold_FableLimitStaysOnOwnSnapshotCopy|SchedulerCacheSnapshotMemo)' -count=1 -timeout=10m
```

远程 CI 按维护分支的对应提交核验，本地通过不代表生产环境已验收。

本次不执行生产部署。正式制品和生产验收分别记录；服务器的实际版本、迁移历史及配置在后续部署前重新核验。
