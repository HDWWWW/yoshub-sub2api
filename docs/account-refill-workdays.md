# 定时强制补号：中国法定工作日

在「补号规则 → 每日定时强制补号」中，每条任务新增「执行日期」：

- **每天**：保留原有行为。
- **中国法定工作日（含调休补班）**：按北京时间判断。普通周末及国务院公布的休假日跳过；周末调休补班日正常执行。

每个分组、每条任务独立选择。已有任务默认仍为每天。保存工作日选项并启用任务后，后台自动获取日历，无需逐年改配置。任务区域显示已同步年份、最近成功同步时间、同步结果和下次执行时间。

## 年度数据与自动同步

使用 [holiday-cn](https://github.com/NateScarlet/holiday-cn) 提供的 JSON。该项目从国务院公告整理节假日和调休安排，数据中的 `papers` 保存政府公告链接；它是第三方数据源，并非政府 API。

启用工作日任务后立即唤醒同步，随后每 24 小时检查**当年和次年**。跨年后自动滚动年份，次年尚未公布时持续检查；每次同步会重新计算下次执行时间。次年文件对当年 12 月的调整优先应用。

- 主源：`https://raw.githubusercontent.com/NateScarlet/holiday-cn/master/{year}.json`
- 网络或数据异常时备用：`https://cdn.jsdelivr.net/gh/NateScarlet/holiday-cn@master/{year}.json`
- 校验年份、日期、布尔调休标识、重复日期、政府公告 URL，以及响应体积。
- 日历随补号规则持久化，重启后继续可用。网络读取在独立后台任务进行，不占用补号下单锁；同一轮各分组共用年度下载结果。
- 同步失败保留已缓存数据，1 小时后重试。缺少目标年度日历时，该工作日任务暂停并显示原因，不回退为普通周一至周五。

2026-11-09 核对时，数据源已提供 2026、2027 年安排。2027 年公告：[国务院办公厅关于2027年部分节假日安排的通知](https://www.gov.cn/zhengce/zhengceku/202611/content_7085102.htm)。未来年份需等待官方公布及数据源更新后才能同步。

## 执行与兼容

每条工作日任务每个北京时间日期最多创建一批。休息日不创建定时批次；停机恢复仅补当天已经到时间的任务，不追补往日漏单。首次启用以及首次日历就绪不会立即追补已过时刻。已创建订单继续原有处理流程。

任务的套餐、数量、预算、推送、幂等及普通自动补号规则维持既有语义。修改时间或日期模式仍保留当天执行记录。旧版客户端省略 `day_mode` 时，按任务 ID 保留已设置的工作日限制；显式设置 `""` 可恢复每天执行。

配置增加可选字段 `daily_schedules[].day_mode`（或旧单任务 `daily_schedule.day_mode`），取值 `""` / `"china_workday"`。状态接口增加 `workday_calendar`，包括 `years`、`synced_at`、`checked_at`、`message`。

## 本地验证

覆盖真实年度日历的法定休假、调休周末、普通工作日、北京时间跨日、跨年及次年对 12 月的覆盖；模拟断网和年度未发布；验证重启、任务独立性、当天去重及旧客户端保存兼容。所有购买测试均使用本地测试号商。

```powershell
# backend
go test -tags=unit ./internal/service -run 'Test.*Refill' -count=1
go test -tags=unit ./internal/handler/admin -run 'Test(Refill|DecodeAccountRefill)' -count=1
go test -race -tags=unit ./internal/service -run 'TestRefill(Calendar|Workday|Daily|MultipleDaily)' -count=1
go build -buildvcs=false -tags=embed ./cmd/server

# frontend
node node_modules/vitest/vitest.mjs run src/components/admin/account/__tests__/AccountRefillModal.spec.ts src/i18n/__tests__
node node_modules/vue-tsc/bin/vue-tsc.js --noEmit
node node_modules/vite/bin/vite.js build
```
