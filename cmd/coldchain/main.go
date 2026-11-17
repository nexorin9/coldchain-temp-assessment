// coldchain 是药品与疫苗冷链温控判定服务的命令行入口。
//
// 同一二进制承担两个角色：
//   - serve：常驻的 HTTP API 服务（院内内网小主机上跑，供记录仪 / 边缘网关投递事件、
//     供冷链管理员与主管查判定与确认），这是产品的常态运行形态；
//   - assess / import / replay / archive / export / bench：运维与批处理通道，
//     供无人值守跑批、离线回放与周报导出使用。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chaosforge/coldchain-temp-assessment/internal/assessment"
	"github.com/chaosforge/coldchain-temp-assessment/internal/audit"
	"github.com/chaosforge/coldchain-temp-assessment/internal/bench"
	"github.com/chaosforge/coldchain-temp-assessment/internal/config"
	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/httpapi"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ledger"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/model"
	"github.com/chaosforge/coldchain-temp-assessment/internal/observe"
	"github.com/chaosforge/coldchain-temp-assessment/internal/report"
	"github.com/chaosforge/coldchain-temp-assessment/internal/rules"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
	"github.com/chaosforge/coldchain-temp-assessment/internal/store"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// version 是服务版本号。
const version = "0.1.0"

// shapeRegistryBuilder 装配本次启动用的判定结果形状注册表。
//
// 做成包级变量：测试可以换上带冲突的一份，断言「重复登记同名结果类型时
// 启动直接失败」，而不是带着形状冲突的服务对外服务。
var shapeRegistryBuilder = func() (*runtime.ResultShapeRegistry, error) {
	return runtime.DefaultResultShapeRegistry()
}

// 退出码约定。
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitConfig  = 78
)

// 存储层实现报表取数读侧——这里钉一个编译期断言：
// 报表包只声明「要哪几份数据」，不关心怎么拿；
// 一旦存储层改了某个方法的签名，这条断言会先于运行时报错。
var _ report.Reader = (*store.Store)(nil)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 是真正的入口逻辑，便于测试直接调用。
func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("coldchain-temp-assessment %s\n", version)
		return exitOK
	case "help", "--help", "-h":
		usage()
		return exitOK
	case "assess":
		return runAssess(args[1:])
	case "import":
		return runImport(args[1:])
	case "replay":
		return runReplay(args[1:])
	case "draft":
		return runDraft(args[1:])
	case "batches":
		return runBatches(args[1:])
	case "export":
		return runExport(args[1:])
	case "archive":
		return runArchive(args[1:])
	case "bench":
		return runBench(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", args[0])
		usage()
		return exitUsage
	}
}

// runServe 启动常驻 HTTP 服务：先做严格配置校验，再监听端口。
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "HTTP 监听地址")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	logger := logging.Default()

	// 启动即失败校验：判定结果的形状必须唯一，且每一步都有已登记的结果形状。
	// 形状冲突意味着同一个结果类型名底下挂着两种形状——带着这样的结论去写台账，
	// 解析器多半不会报错，而是安静地读出半个结论。那比服务起不来坏得多，
	// 所以冲突在这里就拦下：打印冲突名，以非 0 退出码退出，不监听端口。
	reg, err := shapeRegistryBuilder()
	if err != nil {
		e := errcode.As(err)
		fmt.Fprintf(os.Stderr, "判定结果形状校验失败：%s（%s）\n", e.Message, e.Details)
		return exitConfig
	}
	if err := runtime.ValidateStartupShapes(reg); err != nil {
		e := errcode.As(err)
		fmt.Fprintf(os.Stderr, "判定结果形状校验失败：%s（%s）\n", e.Message, e.Details)
		return exitConfig
	}

	cfg, err := config.Load()
	if err != nil {
		// 缺配置即启动失败：不回落到本地替身，避免「看着跑通了、实际没接院内系统」。
		e := errcode.As(err)
		fmt.Fprintf(os.Stderr, "配置校验失败：%s\n", e.Error())
		return exitConfig
	}
	logger.Info("serve_start", "version", version, "config", cfg.Describe())

	// 本地存储：温度事件落在这里，是后面所有判定结论的原始依据。
	// 库打不开就起不来——没有库的判定服务等于没接院内系统。
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		e := errcode.As(err)
		fmt.Fprintf(os.Stderr, "打开本地存储失败：%s（%s）\n", e.Message, e.Details)
		return exitConfig
	}
	defer func() { _ = st.Close() }()

	// 入站限流：按点位限速，超限的投递显式拒绝并记录，不静默丢弃。
	limiter := ingest.NewRateLimiter(cfg.IngestRateLimit, cfg.IngestRateWindow, cfg.IngestRateBurst)
	ingestor := ingest.NewIngestor(st, limiter, logger)

	// 审计链：每次模型调用的元数据（模型、耗时、token、估算成本）落在这里，
	// 供内审与飞检经 GET /v1/runs/{id}/audit 调阅。
	//
	// 单价表没配就把成本记成 unknown——「本服务不知道单价」，
	// 而不是「这次调用不花钱」。登记的凭据原文在这里被抹掉，
	// 审计记录里不留 api_key、鉴权头与提示词原文。
	auditRecorder := audit.NewRecorder(st,
		audit.WithPricer(loadPricer(cfg)),
		audit.WithSecrets(cfg.ModelAPIKey, cfg.IngestToken, cfg.OperatorToken,
			cfg.SupervisorToken, cfg.AuditorToken, cfg.LedgerToken, cfg.NotifyToken),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 出站写回的两端：冷链台账的越温偏差写入、接种门诊通知。
	// 装配失败即启动失败——带着半份配置去发一次注定失败的写请求，
	// 得到的 401 会掩盖真正的问题（配置没给，而不是端点坏了）。
	writebackCfg, err := ledger.WritebackConfigFromEnv()
	if err != nil {
		e := errcode.As(err)
		fmt.Fprintf(os.Stderr, "冷链台账写侧配置校验失败：%s\n", e.Error())
		return exitConfig
	}
	deviations, err := ledger.NewDeviationWriter(writebackCfg)
	if err != nil {
		e := errcode.As(err)
		fmt.Fprintf(os.Stderr, "装配冷链台账写侧失败：%s\n", e.Error())
		return exitConfig
	}
	notifyCfg, err := ledger.NotifyConfigFromEnv()
	if err != nil {
		e := errcode.As(err)
		fmt.Fprintf(os.Stderr, "通知系统配置校验失败：%s\n", e.Error())
		return exitConfig
	}
	notifier, err := ledger.NewNotifier(notifyCfg)
	if err != nil {
		e := errcode.As(err)
		fmt.Fprintf(os.Stderr, "装配通知系统失败：%s\n", e.Error())
		return exitConfig
	}
	// 出站退避重试：台账写不进去、门诊通知发不出，都是能靠「再敲一次门」
	// 解决的瞬时故障。重试耗尽才挂起待补偿——那才是要人管的事。
	retryPolicy := ledger.RetryPolicy{
		MaxRetries: cfg.WritebackMaxRetries,
		BaseDelay:  cfg.WritebackBackoff,
		MaxDelay:   ledger.DefaultRetryMaxDelay,
	}
	retryingDeviations := ledger.NewRetryingDeviationWriter(deviations, retryPolicy, logger)
	retryingNotifier := ledger.NewRetryingNotifier(notifier, retryPolicy, logger)
	// 「未确认不得写回」那道闸门包在台账写侧外面：
	// run 不到已确认，一次出站写请求都不发。
	gatedWriter := workflow.NewWritebackGate(retryingDeviations, st)
	writebacker := runtime.NewWritebacker(st, gatedWriter, retryingNotifier, logger)
	// 补偿重放：写回没落成的越温进待补偿清单，等人来补发。
	compensator := runtime.NewCompensator(st, writebacker, logger)

	// 启动续跑扫描：上次进程是在判定跑到一半时消失的（断电、发布、巡检重启），
	// 库里留着那条没跑完的判定。这里把它接着跑完——已完成的步骤不重跑，
	// 已经写过台账的也不再写一次。
	if summary, err := resumePendingRuns(ctx, st, logger, auditRecorder); err != nil {
		logger.Error("resume_scan_failed", "error", err.Error())
	} else {
		logger.Info("resume_scan_completed",
			"scanned", summary.Scanned,
			"resumed", summary.Resumed,
			"skipped", summary.Skipped,
			"failed", summary.Failed)
	}

	// 小时级聚合刷新：运行摘要的逐小时分解与周报都读这张表。
	// 原始采样留在线上热表里，聚合表把「这一小时有几条采样、峰值多少」
	// 定格下来——汇总与报表因此不必扫原始采样。
	// 间隔取 10 分钟：够密到摘要里的逐小时分解不会太旧，
	// 也够疏到不去跟主路径抢写锁。
	go refreshAggregatesLoop(ctx, st, logger)

	// 运行摘要：给运维一个「现在有事吗」的入口。
	// 它是只读的——不推进任何业务状态，也不发任何出站请求。
	summaryCollector := observe.NewCollector(observe.NewStoreSource(st))

	srv := &http.Server{
		Addr: *addr,
		Handler: httpapi.New(httpapi.Options{
			Version:   version,
			Logger:    logger,
			Ingestor:  ingestor,
			Importer:  ingestor,
			RunReader: st,
			// 判定台账清单查询：与 run 详情同源，取的是同一张 runs 表。
			RunLister: st,
			// 点位温区配置登记簿：改的是判定口径，写入前先过 rules.ValidatePointProfile。
			PointProfiles: st,
			AuditReader:   st,
			// 确认编排器：主管确认 / 否决时推进状态、固化台账写回载荷、记审计链。
			// 它不出站——台账写入是另一段流程的事，且必须先过「未确认不得写回」那道闸门。
			Confirmer: runtime.NewConfirmer(st, auditRecorder),
			// 出站写回：确认成立之后把这条越温写进冷链台账、通知接种门诊。
			Writebacker: writebacker,
			// 补偿重放：写回没落成的越温进待补偿清单，等人来补发。
			Compensator: compensator,
			// 运行摘要：近 24 小时的判定与模型调用、此刻欠着的写回与待确认。
			// 只读，不推进任何业务状态。
			SummaryReader: summaryCollector,
			IngestToken:   cfg.IngestToken,
			// 角色凭据表：一把令牌一个身份。角色闸门据此判断
			// 「你是谁」与「你能不能做这件事」——凭据只管进门，
			// 权限才管做事。
			Credentials: []httpapi.Credential{
				{Role: workflow.RoleOperator, Token: cfg.OperatorToken},
				{Role: workflow.RoleSupervisor, Token: cfg.SupervisorToken},
				{Role: workflow.RoleAuditor, Token: cfg.AuditorToken},
			},
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("serve_failed", "error", err.Error())
		fmt.Fprintf(os.Stderr, "服务启动失败：%v\n", err)
		return exitFailure
	case <-ctx.Done():
		logger.Info("serve_stopping")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("serve_shutdown_failed", "error", err.Error())
		return exitFailure
	}
	logger.Info("serve_stopped")
	return exitOK
}

// aggregateRefreshInterval 是小时级聚合的重算间隔。
//
// 10 分钟是权衡出来的：够密到运行摘要里的逐小时分解不会太旧，
// 也够疏到不去跟主路径抢写锁。聚合只读近 24 小时的采样，
// 一次重算的量是「一天的采样」，不是全库。
const aggregateRefreshInterval = 10 * time.Minute

// aggregateRefreshWindow 是每次重算覆盖的区间长度。
const aggregateRefreshWindow = 24 * time.Hour

// refreshAggregatesLoop 定期重算近 24 小时的小时级聚合。
//
// 为什么要这个循环：运行摘要的逐小时分解与周报都读 event_hourly_agg，
// 而这张表不是每次写入时同步维护的——它是按小时归桶的统计，
// 边写边算会把同一个桶反复改写。它由这个循环按固定间隔补齐：
// 原始采样仍在线上热表里，聚合表把「这一小时有几条采样、峰值多少」定格。
//
// 失败不打断服务：聚合只是报表与摘要的口径，缺了就少一段分解，
// 不该让服务起不来。失败记一条 warn，下一轮再试。
func refreshAggregatesLoop(ctx context.Context, st *store.Store, logger *logging.Logger) {
	refresh := func() {
		to := time.Now().UTC()
		from := to.Add(-aggregateRefreshWindow)
		written, err := st.RefreshHourlyAggregates(ctx, "", from, to, store.FixedZoneOffset)
		if err != nil {
			logger.Warn("aggregate_refresh_failed", "error", err.Error())
			return
		}
		logger.Info("aggregate_refreshed",
			"buckets", written,
			"window_hours", int(aggregateRefreshWindow/time.Hour))
	}

	// 启动即先补一次：服务刚起来时摘要里就该有逐小时分解，
	// 不必等满一个周期。
	refresh()

	ticker := time.NewTicker(aggregateRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// resumePendingRuns 扫描库里没跑完的判定并接着跑。
//
// 续跑用的判定链按每条 run 持久化的温区配置重建——用的是「当初那一份」，
// 不是现在的配置：温区口径变了，重跑出来的结论就不是同一条判定了。
// 取数步骤从本地库按窗口区间回读采样，与在线判定走的是同一段取数代码。
func resumePendingRuns(ctx context.Context, st *store.Store, logger *logging.Logger,
	recorder audit.AuditRecorder) (runtime.ResumeSummary, error) {
	resumer := runtime.NewResumer(st, func(profile *domain.PointProfile) (*runtime.Executor, error) {
		chain, err := runtime.NewChain(runtime.ChainDeps{
			Profile:       profile,
			Source:        st,
			AuditRecorder: recorder,
		})
		if err != nil {
			return nil, err
		}
		reg, err := shapeRegistryBuilder()
		if err != nil {
			return nil, err
		}
		if err := runtime.ValidateChainShapes(chain, reg); err != nil {
			return nil, err
		}
		exec := runtime.NewExecutor(chain, st, logger).WithShapeRegistry(reg)
		exec.WithRunPersistence(st, profile)
		return exec, nil
	}, logger)
	return resumer.ResumeAll(ctx)
}

// loadPricer 按配置装配计价器。
//
// 配了单价表就按表估成本；没配（或表读不出来）就用 UnknownPricer——
// 成本记 unknown，而不是记 0。「这次调用不花钱」是一句结论，
// 本服务在不知道单价时没有资格下这个结论。
//
// 单价表读不出来不阻断启动：它只影响成本这一个字段，
// 不影响判定本身。读不出来的原因记进启动日志，别让人以为表配好了。
func loadPricer(cfg *config.Config) audit.Pricer {
	if cfg == nil || strings.TrimSpace(cfg.ModelPriceTable) == "" {
		return audit.UnknownPricer{}
	}
	table, err := audit.LoadPriceTable(cfg.ModelPriceTable)
	if err != nil {
		logging.Default().Error("model_price_table_unreadable",
			"path", cfg.ModelPriceTable, "error", err.Error())
		return audit.UnknownPricer{}
	}
	logging.Default().Info("model_price_table_loaded",
		"path", cfg.ModelPriceTable, "entries", table.Len())
	return audit.NewTablePricer(table)
}

// runAssess 对温度事件做判定。
//
// 三种形态：
//   - 只给 --events：打印规范化后的判定输入 JSON（接入段产物）；
//   - 同时给 --profile：走完整的最窄判定主路径，打印 {run, window, zone} 三段 JSON
//     （建 run → 温区闸门 → 抽越限窗口）；
//   - 再给 --batches：把在库批次清单一起带上，判定会把批次挂到各自的制品温区下，
//     并当场发现「在库有货却查不到温区」这种说不清的情况。
//
// stdout 只出 JSON（可直接被管道消费），摘要与错误写 stderr，失败即非 0 退出码。
func runAssess(args []string) int {
	fs := flag.NewFlagSet("assess", flag.ContinueOnError)
	eventsPath := fs.String("events", "", "温度事件 JSON 文件路径（必填）")
	profilePath := fs.String("profile", "", "点位温区配置 JSON 文件路径（给出后执行完整判定）")
	batchesPath := fs.String("batches", "", "在库批次清单 CSV 文件路径（给出后把在库批次挂到判定链上）")
	withSteps := fs.Bool("steps", false, "跑完整判定步骤链（取数→抽窗口→归因→成稿→待确认）并打印步骤时间线")
	useModel := fs.Bool("model", false, "归因走模型（端点取自 MODEL_BASE_URL / MODEL_API_KEY / MODEL_NAME）；模型不可达时自动降级到规则基线")
	useLedger := fs.Bool("ledger", false, "在库批次清单走冷链台账读侧（基址与凭据取自 COLDCHAIN_LEDGER_BASE_URL / COLDCHAIN_LEDGER_TOKEN）")
	dbPath := fs.String("db", "", "本地库路径（配合 --steps 时把每步产物落进 run_steps 表；默认不落库）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *eventsPath == "" {
		fmt.Fprintln(os.Stderr, "错误：assess 需要 --events <file> 指定温度事件 JSON 文件")
		return exitUsage
	}

	if *profilePath == "" {
		// 温区配置有两个来源：--profile 指向的本地文件，或登记簿（--db）。
		// 登记簿这一条是给现场的常态用法：配置在服务里登记一次，
		// 之后每台机器跑批都按同一份口径判，不必各自拷一份 JSON。
		//
		// 只有多点位采样序列才走登记簿：单条事件文件没有「这一批属于哪个点位」
		// 的上下文，从登记簿里定位点位是猜。
		if strings.TrimSpace(*dbPath) != "" {
			if series, err := ingest.LoadEventSeries(*eventsPath); err == nil && len(series) > 0 {
				profile, err := profileFromRegistry(*dbPath, series)
				if err != nil {
					fmt.Fprintf(os.Stderr, "错误：%v\n", err)
					return exitFailure
				}
				return assessRun(*eventsPath, profile, *batchesPath, *withSteps, *dbPath, *useModel, *useLedger)
			}
		}
		if *withSteps {
			fmt.Fprintln(os.Stderr, "错误：--steps 需要温区配置：给出 --profile <file>，或先在登记簿里登记该点位的配置再带 --db <db>")
			return exitUsage
		}
		return assessNormalize(*eventsPath)
	}
	profile, err := ingest.LoadProfileFile(*profilePath)
	if err != nil {
		return reportError(err)
	}
	if *useModel && !*withSteps {
		fmt.Fprintln(os.Stderr, "注意：--model 只在 --steps 判定步骤链下生效（归因那一步才走模型端点），本次按规则基线归因执行")
	}
	if *useLedger && !*withSteps {
		fmt.Fprintln(os.Stderr, "注意：--ledger 只在 --steps 判定步骤链下生效（在库批次清单在成稿那一步才需要），本次按本地清单执行")
	}
	if *useLedger && strings.TrimSpace(*batchesPath) != "" {
		fmt.Fprintln(os.Stderr, "注意：--ledger 与 --batches 同时给出时以 --batches 的本地清单为准，本次不查台账")
		*useLedger = false
	}
	return assessRun(*eventsPath, &profile, *batchesPath, *withSteps, *dbPath, *useModel, *useLedger)
}

// profileFromRegistry 从点位温区配置登记簿里取该点位的配置。
//
// 点位从采样里认：登记簿按 point_id 索引，而 point_id 是采样自带的。
// 登记簿里没有这份配置时明确说没有——不猜温区、不用默认值顶替，
// 也不拿「最近一个点位的配置」凑合。
func profileFromRegistry(dbPath string, series []ingest.RawEvent) (*domain.PointProfile, error) {
	if len(series) == 0 {
		return nil, fmt.Errorf("事件文件里没有采样，无法确定点位")
	}
	pointID := strings.TrimSpace(series[0].PointID)
	if pointID == "" {
		return nil, fmt.Errorf("采样里没有 point_id，无法确定点位")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开点位温区配置登记簿失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	profile, err := st.PointProfileByID(context.Background(), pointID)
	if err != nil {
		e := errcode.As(err)
		return nil, fmt.Errorf("点位温区配置登记簿里没有点位 %s 的配置（%s），系统不猜测温区、不用默认值顶替；"+
			"请用 --profile <file> 给出本地配置，或先登记该点位的配置再重跑", pointID, e.Message)
	}
	return &profile, nil
}

// assessNormalize 只做接入段：读入一条事件、逐字段校验、打印规范化判定输入。
func assessNormalize(eventsPath string) int {
	raw, err := ingest.LoadEventFile(eventsPath)
	if err != nil {
		return reportError(err)
	}
	event, err := ingest.NormalizeEvent(raw)
	if err != nil {
		return reportError(err)
	}

	payload, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：判定输入序列化失败：%v\n", err)
		return exitFailure
	}
	fmt.Println(string(payload))
	fmt.Fprintf(os.Stderr, "已规范化 1 条温度事件：点位 %s，测量时间 %s，温度 %.2f℃\n",
		event.PointID, event.MeasuredAt.Format(time.RFC3339), event.TemperatureC)
	// 精度不足要在摘要里就看见：这条读数接下来会被拿去做归因与成稿，
	// 只藏在 JSON 里的话，用命令跑一遍的人根本不会注意到它只能参考。
	if event.Precision.Limited {
		fmt.Fprintf(os.Stderr, "注意：%s——%s\n", event.Precision.Tag, event.Precision.Detail)
	}
	return exitOK
}

// assessRun 走最窄判定主路径：读入采样 → 规范化 → 建 run → 温区闸门 → 抽越温窗口 → 打印结论。
//
// profile 是本次判定采用的温区配置，来自本地文件或登记簿（调用方决定）。
// 温区配置缺失或不匹配时，系统判不了：仍打印一条 state=needs_manual 的 run，
// 让现场看到「系统判不了、为什么判不了」，但以非 0 退出码收尾——
// 判定没完成就是没完成，不能让无人值守的批处理把它当成成功。
//
// withSteps 为真时改跑完整判定步骤链（见 runStepChain）。
func assessRun(eventsPath string, profile *domain.PointProfile, batchesPath string, withSteps bool, dbPath string, useModel, useLedger bool) int {
	if profile == nil {
		fmt.Fprintln(os.Stderr, "错误：判定需要点位温区配置")
		return exitUsage
	}
	raws, err := ingest.LoadEventSeries(eventsPath)
	if err != nil {
		return reportError(err)
	}

	samples := make([]domain.TemperatureEvent, 0, len(raws))
	for _, raw := range raws {
		event, err := ingest.NormalizeEvent(raw)
		if err != nil {
			return reportError(err)
		}
		samples = append(samples, event)
	}

	// 在库批次清单（可选）：批次号只由台账给出，这里只读不改。
	var batches []domain.Batch
	if strings.TrimSpace(batchesPath) != "" {
		batches, err = ingest.LoadBatchFile(batchesPath)
		if err != nil {
			return reportError(err)
		}
	}

	result, assessErr := assessment.AssessWithBatches(samples, profile, batches)

	if withSteps {
		if assessErr != nil {
			// 最窄判定都没过（温区缺失、点位对不上），步骤链没有跑的基础。
			e := errcode.As(assessErr)
			fmt.Fprintf(os.Stderr, "判定未完成（run %s，状态 %s）：%s（%s）\n",
				result.Run.RunID, result.Run.State, e.Message, e.Details)
			return exitFailure
		}
		return runStepChain(result, samples, profile, batches, dbPath, useModel, useLedger)
	}

	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：判定结论序列化失败：%v\n", err)
		return exitFailure
	}
	fmt.Println(string(payload))

	if assessErr != nil {
		e := errcode.As(assessErr)
		fmt.Fprintf(os.Stderr, "判定未完成（run %s，状态 %s）：%s（%s）\n",
			result.Run.RunID, result.Run.State, e.Message, e.Details)
		return exitFailure
	}

	if result.Window == nil {
		// 没有窗口有两种情形，摘要必须分开说：
		// 「压根没越限」和「越了但没够最短时长门槛」对药师是两回事，
		// 混成一句「均未越限」会把一次真实越限读数说没了。
		if result.Excursions != nil && result.Excursions.SegmentCount > 0 {
			fmt.Fprintf(os.Stderr,
				"判定完成（run %s）：点位 %s 的 %d 条采样中有 %d 条越限（%d 段），但确认时长 %d 分钟未达最短越限门槛 %d 分钟，未生成越温窗口\n",
				result.Run.RunID, result.Run.PointID, len(samples),
				result.Excursions.OutOfRangeSamples, result.Excursions.SegmentCount,
				result.Excursions.LongestConfirmedMinutes, result.Excursions.MinExcursionMinutes)
			// 短时波动单列：它确实发生过，只是不够长。
			for _, tr := range result.TransientExcursions {
				fmt.Fprintf(os.Stderr, "  短时波动 %s ~ %s：峰值 %.2f℃，持续 %d 分钟（%s）\n",
					tr.Start.Format(time.RFC3339), tr.End.Format(time.RFC3339),
					tr.PeakC, tr.DurationMinutes, tr.Reason)
			}
		} else {
			fmt.Fprintf(os.Stderr, "判定完成（run %s）：点位 %s 的 %d 条采样均未越限，未生成越温窗口\n",
				result.Run.RunID, result.Run.PointID, len(samples))
		}
		printZoneSummary(result.Zone)
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "判定完成（run %s）：越温窗口 %s ~ %s，峰值 %.2f℃，持续 %d 分钟，采样 %d 条\n",
		result.Run.RunID,
		result.Window.Start.Format(time.RFC3339),
		result.Window.End.Format(time.RFC3339),
		result.Window.PeakC,
		result.Window.DurationMinutes,
		result.Window.SampleCount)
	// 合并窗口要说明白：它由几段越限合成、中间回落了多久。
	// 「一次异常中间回落到温区 5 分钟」和「一次异常全程没回落」不是一回事，
	// 前者往往说明设备在反复启停，该查的是设备而不是那批疫苗。
	if result.Window.SegmentCount > 1 {
		fmt.Fprintf(os.Stderr, "该窗口由 %d 段相邻越限合并而成，中间确认回落温区 %d 分钟（不计入越温时长）\n",
			result.Window.SegmentCount, result.Window.RecoveryMinutes)
	}
	printZoneSummary(result.Zone)
	return exitOK
}

// printZoneSummary 把温区判定结论摘要打到 stderr。
//
// 温区来源、精度与绑定状态这三样必须让人一眼看见：
// 「2~8℃」这个数字本身不说明任何事，说清它是谁定的、精度够不够、
// 设备核验到哪一步，结论才敢往台账上写。
func printZoneSummary(zone *rules.ZoneDecision) {
	if zone == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "温区：%s（来源 %s，%s）\n", zone.ZoneLabel, zone.ZoneSource, zone.ZoneBasis)
	for _, p := range zone.Products {
		mark := "在温区内"
		if p.OutOfRange {
			mark = "已越限"
		}
		line := fmt.Sprintf("  制品 %s：温区 %s（%s），当前读数%s", p.ProductName, p.ZoneLabel, p.ZoneBasis, mark)
		if len(p.BatchRefs) > 0 {
			line += fmt.Sprintf("，在库批次 %s", strings.Join(p.BatchRefs, "、"))
		}
		fmt.Fprintln(os.Stderr, line)
	}
	if zone.Precision.Limited {
		fmt.Fprintf(os.Stderr, "注意：%s——%s\n", zone.Precision.Tag, zone.Precision.Detail)
	}
	if len(zone.Binding.UnmappedProducts) > 0 {
		fmt.Fprintf(os.Stderr, "注意：在库批次有制品没有对应温区配置：%s\n",
			strings.Join(zone.Binding.UnmappedProducts, "、"))
	}
	if zone.Binding.Reason != "" {
		fmt.Fprintf(os.Stderr, "绑定：%s（%s）\n", zone.Binding.Status, zone.Binding.Reason)
	}
}

// assessStepsOutput 是 --steps 模式的输出：run、越温窗口、步骤时间线与按分码取回的产物。
type assessStepsOutput struct {
	Run      domain.Run              `json:"run"`
	Window   *domain.ExcursionWindow `json:"window"`
	Steps    []runtime.StepTimeline  `json:"steps"`
	Products map[string]any          `json:"products"`
}

// runStepChain 跑完整判定步骤链并打印时间线。
//
// 它走的是与常驻服务同一条执行器代码路径（internal/runtime 的执行器与步骤链），
// 不是另写一套演示逻辑：命令行与服务端跑出来的步骤次序、产物分码与失败语义完全一致。
//
// 给出 --db 时每步产物落进 run_steps 表，时间线之后可回看；不给则只在内存里走一遍。
//
// 给出 --model 时归因走模型端点；模型不可达会自动降级到规则基线，
// 结论停在待主管确认，不写台账（见 internal/model/degrade.go）。
func runStepChain(result assessment.Result, samples []domain.TemperatureEvent, profile *domain.PointProfile, batches []domain.Batch, dbPath string, useModel, useLedger bool) int {
	var recorder runtime.StepRecorder
	var runRecorder runtime.RunRecorder
	var storeHandle *store.Store
	if strings.TrimSpace(dbPath) != "" {
		st, err := store.Open(dbPath)
		if err != nil {
			return reportError(err)
		}
		defer func() { _ = st.Close() }()
		recorder = st
		runRecorder = st
		storeHandle = st
	}

	deps := runtime.ChainDeps{Profile: profile, Samples: samples, Batches: batches}
	if useModel {
		handler, err := modelAttributionHandler(profile, storeHandle)
		if err != nil {
			return reportError(err)
		}
		deps.Attribute = handler
	}
	// 在库批次读侧：成稿那一步按越温窗口起点去冷链台账查在库批次。
	// 台账没查成不是判定失败——成稿会写明「批次信息缺失，待人工核对」。
	if useLedger {
		cfg, err := ledger.ConfigFromEnv()
		if err != nil {
			return reportError(err)
		}
		client, err := ledger.NewClient(cfg)
		if err != nil {
			return reportError(err)
		}
		deps.BatchReader = client
	}

	chain, err := runtime.NewChain(deps)
	if err != nil {
		return reportError(err)
	}

	// 跑链之前先校验结果形状：每一步的结果分码都要已登记，
	// 且同一个分码不得被两步共用。命令行与服务端走同一份校验，
	// 不会出现「命令行跑得通、服务端跑不通」。
	reg, err := shapeRegistryBuilder()
	if err != nil {
		return reportError(err)
	}
	if err := runtime.ValidateChainShapes(chain, reg); err != nil {
		return reportError(err)
	}

	executor := runtime.NewExecutor(chain, recorder, logging.Default()).WithShapeRegistry(reg)
	// 给出 --db 时 run 状态也一并落库：进程重启后靠它认出「这条判定跑到哪一步了」，
	// 并按已成功的步骤产物接着跑，而不是从头再判一遍。
	if runRecorder != nil {
		executor.WithRunPersistence(runRecorder, profile)
	}
	exec, execErr := executor.Run(context.Background(), result.Run)

	out := assessStepsOutput{
		Run:      exec.Run,
		Window:   result.Window,
		Steps:    runtime.Timeline(exec.Steps),
		Products: map[string]any{},
	}
	for _, kind := range domain.PayloadKinds() {
		if value, ok := exec.Product(kind); ok {
			out.Products[string(kind)] = value
		}
	}

	payload, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：判定步骤链结果序列化失败：%v\n", err)
		return exitFailure
	}
	fmt.Println(string(payload))

	if execErr != nil {
		e := errcode.As(execErr)
		fmt.Fprintf(os.Stderr, "判定步骤链未完成（run %s，停在步骤 %s，执行态 %s）：%s（%s）\n",
			exec.Run.RunID, exec.FailedStep, exec.Run.ExecState, e.Message, e.Details)
		return exitFailure
	}
	// 降级要显眼：这条结论不是模型给的，递到药事与企业面前之前必须有人重点核对。
	if exec.Run.Degraded {
		fmt.Fprintf(os.Stderr,
			"注意：本次归因已降级（%s）——模型归因不可用，结论由规则基线给出，须由药学部主管重点核对原因判定\n",
			exec.Run.DegradeReason)
	}
	// 批次清单没拿到也要显眼：这份表缺了「涉及批次」一栏，
	// 直接递到药事面前会让人以为这批货不在库里。
	if record, ok := exec.Product(domain.PayloadExceptionRecord); ok {
		if payload, ok := record.(*domain.ExceptionRecordPayload); ok &&
			payload.BatchLookup != nil && payload.BatchLookup.NeedsManual {
			fmt.Fprintf(os.Stderr,
				"注意：%s（%s）——成稿的「涉及批次」一栏须人工到冷链台账核对补齐后再报药事\n",
				domain.BatchMissingText, payload.BatchLookup.Detail)
		}
	}
	fmt.Fprintf(os.Stderr, "判定步骤链完成（run %s）：%d 步全部通过，结论停在「待药学部主管确认」，未触发出站写回\n",
		exec.Run.RunID, len(exec.Steps))
	return exitOK
}

// modelAttributionHandler 装配「模型归因 + 降级」的归因步骤处理器。
//
// 端点、凭据与模型名只从环境变量读：缺任何一项都直接报错退出，
// 不发匿名请求、也不拿本地替身顶替——「看着跑通了、实际没接模型端点」
// 在冷链场景里等于给了一条查不出出处的结论。
func modelAttributionHandler(profile *domain.PointProfile, st *store.Store) (runtime.StepHandler, error) {
	cfg, err := model.GatewayConfigFromEnv()
	if err != nil {
		return nil, err
	}
	gateway, err := model.NewGateway(cfg)
	if err != nil {
		return nil, err
	}
	asker := &model.Asker{Caller: model.GatewayAsker(gateway)}
	if st != nil {
		// 有库时把模型调用记进审计链：这次调用花了多少、成没成、降没降级。
		asker.Auditor = audit.NewRecorder(st)
	}
	return runtime.NewModelAttributionHandler(runtime.ModelAttributionDeps{
		Profile: profile,
		Asker:   asker,
	})
}

// runImport 批量导入记录仪导出的 CSV。
//
// 这是运维与批处理通道上的入口，与 HTTP 端点走的是同一段导入代码
// （internal/ingest 的批量导入）：校验口径、幂等键与单事务落库完全一致，
// 不会出现「命令行导进去的和服务端收的不一样」。
//
// stdout 出 JSON 结果（可被管道消费），逐条跳过原因写 stderr。
// 退出码：0 = 全部落库（或干跑成功）；1 = 有行被跳过或导入失败。
// 有行被跳过时给非 0，是因为这批文件没有完整进库——批处理调度必须能看见，
// 不能让「80 条进、4 条丢」在调度日志里显示成一次成功。
func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	filePath := fs.String("file", "", "记录仪导出的 CSV 文件路径（必填）")
	dryRun := fs.Bool("dry-run", false, "只校验不落库，跳过统计与实跑一致")
	dbPath := fs.String("db", "", "本地库路径（默认取环境变量 DATABASE_PATH）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "错误：import 需要 --file <csv> 指定记录仪导出的 CSV 文件")
		return exitUsage
	}

	data, err := os.ReadFile(*filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return reportError(errcode.Newf(errcode.NotFound, "导入文件不存在：%s", *filePath))
		}
		return reportError(errcode.Newf(errcode.Validation, "读取导入文件失败：%s", err))
	}

	cfg, _ := config.LoadLenient()
	db := cfg.DatabasePath
	if *dbPath != "" {
		db = *dbPath
	}

	st, err := store.Open(db)
	if err != nil {
		return reportError(err)
	}
	defer func() { _ = st.Close() }()

	result, err := ingest.ImportTemperatureSeries(context.Background(), st, data, *dryRun)
	if err != nil {
		return reportError(err)
	}

	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：导入结果序列化失败：%v\n", err)
		return exitFailure
	}
	fmt.Println(string(payload))

	for _, reason := range result.SkipReasons {
		fmt.Fprintf(os.Stderr, "跳过第 %d 行（%s）：%s\n", reason.Row, reason.Code, reason.Reason)
	}

	if result.Skipped > 0 {
		fmt.Fprintf(os.Stderr, "导入完成但有 %d 行被跳过（逐条原因见上），本批未完整入库\n", result.Skipped)
		return exitFailure
	}
	fmt.Fprintf(os.Stderr, "导入完成：共 %d 行，入库 %d 条，重复 %d 条，跳过 %d 条\n",
		result.TotalRows, result.Imported, result.Duplicates, result.Skipped)
	return exitOK
}

// runDraft 打印一条判定已经生成的《温度异常情况记录表》正文。
//
// 这条通道是给现场用的：判定跑完、结论停在待主管确认，
// 药学部要递的那份表就是这一份。稿子从库里按分码取回，
// 打印的是当初判定那一刻渲染出来的正文——
// 不重新渲染、不重新判定，重算一遍就可能出现「打印的稿子与当时的结论对不上」。
//
// stdout 出正文（可直接重定向成附件），提示写 stderr。
// 退出码：0 = 打印成功；1 = 取不到成稿或该判定还没成稿；2 = 用法错误。
func runDraft(args []string) int {
	fs := flag.NewFlagSet("draft", flag.ContinueOnError)
	dbPath := fs.String("db", "", "本地库路径（默认取环境变量 DATABASE_PATH）")
	runID := fs.String("run-id", "", "判定 run 标识（必填）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *runID == "" {
		fmt.Fprintln(os.Stderr, "错误：draft 需要 --run-id <run_id> 指定判定")
		return exitUsage
	}

	cfg, _ := config.LoadLenient()
	db := cfg.DatabasePath
	if *dbPath != "" {
		db = *dbPath
	}

	st, err := store.Open(db)
	if err != nil {
		return reportError(err)
	}
	defer func() { _ = st.Close() }()

	value, ok, err := st.StepPayloadByRunKind(context.Background(), *runID, domain.PayloadExceptionRecord)
	if err != nil {
		return reportError(err)
	}
	if !ok {
		return reportError(errcode.Newf(errcode.NotFound,
			"判定 %s 没有成稿产物：该判定尚未跑到成稿这一步", *runID))
	}
	record, ok := value.(*domain.ExceptionRecordPayload)
	if !ok {
		return reportError(errcode.Newf(errcode.Validation,
			"成稿产物类型不符：%T", value))
	}
	if strings.TrimSpace(record.DraftText) == "" {
		return reportError(errcode.Newf(errcode.NotFound,
			"判定 %s 的成稿正文为空：该判定早于成稿正文能力上线，请重新判定", *runID))
	}

	fmt.Println(record.DraftText)
	return exitOK
}

// runReplay 离线回放历史事件，重建越温窗口并与当初在线判定的结论逐字段比对。
//
// 这是飞检与内审复盘的那条通道：同一批原始读数重跑一遍，
// 窗口起止、峰值、持续分钟、采样条数逐字段对上当初的结论，
// 才对得上「当初那条判定是对的」这句话。
//
// 三条不许让步的约定：
//   - 回放不落库：不写 runs / run_steps / events，不覆盖在线判定记录。
//   - 回放不写回：出站台账写回调用数恒为 0，挂上写回口即拒绝执行。
//   - 回放有独立标记：重建出的 run 标识带 replay_ 前缀，与在线判定一眼可分。
//
// stdout 出 JSON（可被管道消费），摘要与逐字段差异写 stderr。
// 退出码：0 = 比对全部一致；1 = 存在字段级差异或回放失败；2 = 用法错误。
// 有差异时给非 0，是因为这正是复盘要找的东西——批处理调度必须能看见，
// 不能让「结论对不上」在调度日志里显示成一次成功。
func runReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	point := fs.String("point", "", "回放覆盖的点位标识（必填）")
	from := fs.String("from", "", "回放起始时刻，RFC3339 带偏移（必填）")
	to := fs.String("to", "", "回放结束时刻，RFC3339 带偏移（必填）")
	profilePath := fs.String("profile", "", "点位温区配置 JSON（不给时用当初在线判定落库的那一份）")
	dbPath := fs.String("db", "", "本地库路径（默认取环境变量 DATABASE_PATH）")
	archivePath := fs.String("archive", "", "从归档文件读采样（gzip JSONL；不给时从本地库读）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *point == "" {
		fmt.Fprintln(os.Stderr, "错误：replay 需要 --point <point_id> 指定回放覆盖的点位")
		return exitUsage
	}
	if *from == "" || *to == "" {
		fmt.Fprintln(os.Stderr, "错误：replay 需要 --from 与 --to 指定回放区间（RFC3339 带偏移）")
		return exitUsage
	}
	fromAt, err := time.Parse(time.RFC3339, *from)
	if err != nil {
		return reportError(errcode.Newf(errcode.Validation,
			"解析 --from 失败：%s（需 RFC3339 带偏移，如 2026-10-20T03:00:00+08:00）", *from))
	}
	toAt, err := time.Parse(time.RFC3339, *to)
	if err != nil {
		return reportError(errcode.Newf(errcode.Validation,
			"解析 --to 失败：%s（需 RFC3339 带偏移，如 2026-10-20T04:00:00+08:00）", *to))
	}

	// 温区配置：显式给了就用给的那份；不给就用当初在线判定落库的那一份。
	// 系统不猜温区、不用默认值顶替——猜出来的窗口与当初的结论没有可比性。
	var profile *domain.PointProfile
	if strings.TrimSpace(*profilePath) != "" {
		loaded, err := ingest.LoadProfileFile(*profilePath)
		if err != nil {
			return reportError(err)
		}
		profile = &loaded
	}

	cfg, _ := config.LoadLenient()
	db := cfg.DatabasePath
	if *dbPath != "" {
		db = *dbPath
	}
	if strings.TrimSpace(db) == "" {
		return reportError(errcode.Newf(errcode.Validation,
			"未指定本地库路径：请用 --db 指定，或设置环境变量 DATABASE_PATH"))
	}

	st, err := store.Open(db)
	if err != nil {
		return reportError(err)
	}
	defer func() { _ = st.Close() }()

	// 取数来源：默认是热库；给了 --archive 就从归档文件读。
	//
	// 这条分支是归档这件事能不能上线的关键一问：原始采样搬出热表之后，
	// 飞检复盘仍要能把那一段读回来重跑一遍判定。
	// 归档文件与热库是两种来源，但回放的口径完全一样——同一个重放器、同一个温区口径。
	source := runtime.SampleSource(st)
	sourceLabel := "本地库"
	if strings.TrimSpace(*archivePath) != "" {
		archiveSource, err := store.NewArchiveSource(*archivePath)
		if err != nil {
			return reportError(err)
		}
		source = archiveSource
		sourceLabel = *archivePath
	}

	replayer := runtime.NewReplayer(logging.Default())
	result, err := replayer.Replay(context.Background(), runtime.ReplayOptions{
		PointID: *point,
		From:    fromAt,
		To:      toAt,
		Profile: profile,
		Source:  source,
		Online:  st,
	})
	if err != nil {
		return reportError(err)
	}

	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：回放结果序列化失败：%v\n", err)
		return exitFailure
	}
	fmt.Println(string(payload))

	fmt.Fprintf(os.Stderr, "回放完成（点位 %s，%s ~ %s）：回读 %d 条历史采样，重建 %d 段越温窗口，比对 %d 段（一致 %d、不一致 %d、无在线结论可比 %d）\n",
		result.PointID,
		result.From.Format(time.RFC3339),
		result.To.Format(time.RFC3339),
		result.SampleCount,
		result.ReplayedRunCount,
		result.Compared,
		result.Matched,
		result.Mismatched,
		result.Uncompared)
	fmt.Fprintf(os.Stderr, "温区配置来源：%s；采样来源：%s；台账写回调用数 %d（回放不写回台账，也不改写在线判定记录）\n",
		replayProfileSourceLabel(result.ProfileSource), sourceLabel, result.WritebackCalls)

	for _, entry := range result.Windows {
		if entry.Compared && entry.Matched {
			fmt.Fprintf(os.Stderr, "窗口一致：%s ~ %s，峰值 %.2f℃，持续 %d 分钟，采样 %d 条（在线 run %s，业务状态 %s）\n",
				entry.Window.Start.Format(time.RFC3339),
				entry.Window.End.Format(time.RFC3339),
				entry.Window.PeakC,
				entry.Window.DurationMinutes,
				entry.Window.SampleCount,
				entry.OnlineRunID,
				entry.OnlineState)
			continue
		}
		if !entry.Compared {
			fmt.Fprintf(os.Stderr, "未能比对：%s ~ %s——%s\n",
				entry.Window.Start.Format(time.RFC3339),
				entry.Window.End.Format(time.RFC3339),
				entry.CompareNote)
			continue
		}
		fmt.Fprintf(os.Stderr, "窗口不一致：%s ~ %s（在线 run %s），逐字段差异：\n",
			entry.Window.Start.Format(time.RFC3339),
			entry.Window.End.Format(time.RFC3339),
			entry.OnlineRunID)
		for _, diff := range entry.Diffs {
			fmt.Fprintf(os.Stderr, "  %s：回放 %s，在线 %s\n", diff.Field, diff.Replay, diff.Online)
		}
	}

	for _, ref := range result.OnlineWithoutReplay {
		fmt.Fprintf(os.Stderr, "在线有、回放重建不出：%s（业务状态 %s，写回状态 %s）——多半是原始读数缺失或温区口径与本次重跑不同\n",
			ref.RunID, ref.State, ref.WritebackState)
	}

	if result.Mismatched > 0 {
		return exitFailure
	}
	return exitOK
}

// replayProfileSourceLabel 把温区配置来源翻译成人话。
func replayProfileSourceLabel(source string) string {
	switch source {
	case runtime.ProfileSourceOnlineRun:
		return "当初在线判定落库的那一份"
	case runtime.ProfileSourceFile:
		return "本次指定的配置文件"
	default:
		return source
	}
}

// reportError 把带错误码的错误写到 stderr 并返回失败退出码。
func reportError(err error) int {
	e := errcode.As(err)
	fmt.Fprintf(os.Stderr, "错误：%s（%s）\n", e.Message, e.Details)
	return exitFailure
}

// runBench 在公开冷链数据集上跑离线对照基准。
//
// 这是模型归因的对照基准，也是性能基线：
//   - **窗口误差**：规则层抽出的越限窗口，其起止与数据集自带标注段差几条采样点；
//   - **归因 Top-1 一致率**：规则基线给的原因与数据集自带成因标注的一致比例；
//   - **耗时**：读数 + 抽窗口 + 逐窗口归因的总耗时。
//
// 三条不许让步的约定：
//   - **数据集没取到就报错退出**：不拿仓库内的样例行数冒充 40 万条规模，
//     也不拿样例上的指标冒充数据集上的指标。
//   - **结论写回文件**：指标落到 data/bench_baseline.json，供后续模型归因逐条对照。
//   - **不达标给非 0 退出码**：窗口误差或耗时越线时以非 0 退出，
//     让跑批调度看得见，而不是在日志里显示成一次成功。
//
// stdout 出 JSON（可被管道消费），摘要与逐条流明细写 stderr。
// 退出码：0 = 全部达标；1 = 未达标或基准失败；2 = 用法错误。
func runBench(args []string) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	dataset := fs.String("dataset", "data/datasets/vcc-cpld/vaccine_coldchain.csv", "公开冷链数据集 CSV 路径")
	out := fs.String("out", "data/bench_baseline.json", "基准结果落盘路径")
	tolerance := fs.Float64("tolerance-c", bench.DefaultToleranceC, "温区相对设定点的容差（℃）")
	context := fs.Int("context", bench.DefaultContextSamples, "归因时取的窗口前后上下文采样条数")
	minMinutes := fs.Int("min-excursion-minutes", bench.DefaultMinExcursionMinutes, "成立正式越限窗口的最短持续时长（分钟）")
	hysteresis := fs.Float64("hysteresis-c", bench.DefaultHysteresisC, "迟滞带（℃）")
	repeat := fs.Int("repeat", 1, "重复运行次数（≥2 时核对确定性指标是否逐字一致）")
	maxError := fs.Int("max-error-samples", 1, "窗口误差上限（采样条数）")
	maxSeconds := fs.Float64("max-seconds", 30, "耗时上限（秒）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *repeat < 1 {
		fmt.Fprintln(os.Stderr, "错误：--repeat 至少为 1")
		return exitUsage
	}

	opts := bench.Options{
		ToleranceC:          *tolerance,
		ContextSamples:      *context,
		MinExcursionMinutes: *minMinutes,
		HysteresisC:         *hysteresis,
	}

	report, err := bench.RunPipeline(*dataset, opts)
	if err != nil {
		return reportError(err)
	}

	// 重复运行：确定性指标必须逐字一致，不一致说明基准里掺了随机或时序因素。
	if *repeat > 1 {
		first := bench.DeterministicSignature(report)
		for i := 1; i < *repeat; i++ {
			again, err := bench.RunPipeline(*dataset, opts)
			if err != nil {
				return reportError(err)
			}
			same := bench.DeterministicSignature(again) == first
			report.Reproducible = &same
			if !same {
				fmt.Fprintf(os.Stderr, "重复运行结果不一致：\n  第 1 次 %s\n  第 %d 次 %s\n",
					first, i+1, bench.DeterministicSignature(again))
				break
			}
		}
		if report.Reproducible == nil {
			same := true
			report.Reproducible = &same
		}
	}

	if err := writeBenchReport(*out, report); err != nil {
		return reportError(err)
	}

	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：基准报告序列化失败：%v\n", err)
		return exitFailure
	}
	fmt.Println(string(payload))

	fmt.Fprintf(os.Stderr, "基准完成：数据集 %s（%d 行），耗时 %.2fs\n", report.Dataset, report.Records, report.Metrics.ElapsedSeconds)
	fmt.Fprintf(os.Stderr, "指标一 · 窗口误差：最大 %d 个采样周期（平均 %.2f，上限 %d）\n",
		report.Metrics.WindowErrorMaxSamples, report.Metrics.WindowErrorMeanSamples, *maxError)
	fmt.Fprintf(os.Stderr, "指标二 · 归因 Top-1 一致率：%.2f（命中口径 %.2f）\n",
		report.Metrics.CauseTop1AgreementRate, report.Metrics.CauseTop1HitRate)
	fmt.Fprintf(os.Stderr, "指标三 · 耗时：%.2fs（上限 %.0fs）\n", report.Metrics.ElapsedSeconds, *maxSeconds)
	for _, s := range report.Streams {
		fmt.Fprintf(os.Stderr, "  流 %s（%d 条，采样 %ds，温区 %s）：窗口 %d，标注段 %d（对上 %d、未对上 %d），偏差最大 %d；成因标注窗口 %d（唯一 %d，一致 %d）\n",
			s.Name, s.Records, s.SamplingIntervalSec, s.Zone, s.Windows, s.GroundTruthSegments,
			s.MatchedSegments, s.UnmatchedSegments, s.WindowErrorMaxSamples,
			s.CauseLabeledWindows, s.CauseSingleLabelWindows, s.CauseTop1Matched)
	}
	fmt.Fprintf(os.Stderr, "结果已写入 %s\n", *out)

	failed := false
	if report.Metrics.WindowErrorMaxSamples > *maxError {
		fmt.Fprintf(os.Stderr, "未达标：窗口误差 %d 个采样周期 > 上限 %d\n",
			report.Metrics.WindowErrorMaxSamples, *maxError)
		failed = true
	}
	if report.Metrics.ElapsedSeconds > *maxSeconds {
		fmt.Fprintf(os.Stderr, "未达标：耗时 %.2fs > 上限 %.0fs\n",
			report.Metrics.ElapsedSeconds, *maxSeconds)
		failed = true
	}
	if report.Reproducible != nil && !*report.Reproducible {
		fmt.Fprintln(os.Stderr, "未达标：重复运行的确定性指标不一致")
		failed = true
	}
	if failed {
		return exitFailure
	}
	return exitOK
}

// writeBenchReport 把基准报告落盘（目录不存在时先建）。
func writeBenchReport(path string, report *bench.Report) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return errcode.Newf(errcode.StorageError, "创建基准结果目录失败：%s", dir)
		}
	}
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return errcode.Newf(errcode.StorageError, "基准报告序列化失败：%s", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return errcode.Newf(errcode.StorageError, "写基准报告失败：%s", path)
	}
	return nil
}

// runBatches 查冷链台账的在库批次清单。
//
// 这是台账读侧的操作员入口：冷链管理员在处置越温之前，先看一眼
// 「这个点位这个时刻库里到底有什么货」，批次号与数量都直接来自台账，
// 本命令不生成、不改写任何批次号。
//
// stdout 出查询结果 JSON（可被管道消费），stderr 出人话摘要。
// 退出码：0 = 查成了（含确实没有在库批次）；1 = 台账没查成或批次归属有歧义。
func runBatches(args []string) int {
	fs := flag.NewFlagSet("batches", flag.ContinueOnError)
	pointID := fs.String("point", "", "点位标识（必填）")
	at := fs.String("at", "", "查询时刻（RFC3339，含偏移；缺省取当前时间）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*pointID) == "" {
		fmt.Fprintln(os.Stderr, "错误：batches 需要 --point <point_id> 指定点位")
		return exitUsage
	}
	queryAt := time.Now()
	if strings.TrimSpace(*at) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*at))
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：--at 不是 RFC3339 时间（含偏移）：%v\n", err)
			return exitUsage
		}
		queryAt = parsed
	}

	cfg, err := ledger.ConfigFromEnv()
	if err != nil {
		return reportError(err)
	}
	client, err := ledger.NewClient(cfg)
	if err != nil {
		return reportError(err)
	}

	lookup, readErr := client.Batches(context.Background(), *pointID, queryAt)
	payload, err := json.MarshalIndent(lookup, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：在库批次查询结果序列化失败：%v\n", err)
		return exitFailure
	}
	fmt.Println(string(payload))

	if readErr != nil {
		fmt.Fprintf(os.Stderr, "在库批次未取得（%s）：%s\n", lookup.Status, lookup.Detail)
		return exitFailure
	}
	switch lookup.Status {
	case domain.BatchLookupEmpty:
		fmt.Fprintf(os.Stderr, "点位 %s 在 %s 无在库批次（冷链台账查询结果）\n",
			*pointID, queryAt.Format(time.RFC3339))
		return exitOK
	case domain.BatchLookupAmbiguous:
		fmt.Fprintf(os.Stderr, "点位 %s 的在库批次归属有歧义，须人工核对：%s\n", *pointID, lookup.Detail)
		return exitFailure
	default:
		fmt.Fprintf(os.Stderr, "点位 %s 在 %s 有 %d 个在库批次：%s\n",
			*pointID, queryAt.Format(time.RFC3339), len(lookup.Batches), batchRefsText(lookup.Batches))
		return exitOK
	}
}

// batchRefsText 把批次号连成一个可读摘要。
func batchRefsText(batches []domain.Batch) string {
	if len(batches) == 0 {
		return "（无）"
	}
	refs := make([]string, 0, len(batches))
	for _, batch := range batches {
		refs = append(refs, batch.BatchRef)
	}
	return strings.Join(refs, "、")
}

// runExport 导出判定周报 CSV，或小时级聚合 CSV。
//
// 这是药学部冷链管理员每周跑的那条命令：把一周的判定汇成一张能贴进台账的表。
// 出口是 CSV 而不是 JSON，因为周报最终要贴进 Excel 与药事材料；
// 表头固定、中文、带 UTF-8 BOM，双击就能看，不用先转码。
//
// 三条口径写死在 report 包里，这里不重复实现：
//   - 统计范围按越限窗口的起点算（window_start 落在区间内才计入），
//     否则跨周的长段越温会被两周各算一次，各周加起来不等于总数；
//   - 未确认的判定在处置结论一栏写「待确认」，不拿系统建议冒充人的结论；
//   - 缺项就是缺项：没查到批次就是没查到，不用空数组冒充「没有批次」。
//
// stdout 出 CSV（未给 --out 时），摘要写 stderr。
// 退出码：0 = 导出成功；1 = 取数或写文件失败；2 = 用法错误。
func runExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	kind := fs.String("kind", "weekly", "导出内容：weekly=判定周报，hourly=小时级聚合")
	from := fs.String("from", "", "统计区间起点（RFC3339 带偏移，必填）")
	to := fs.String("to", "", "统计区间终点（RFC3339 带偏移，必填）")
	point := fs.String("point", "", "只导某个点位（不给则全部点位）")
	out := fs.String("out", "", "导出文件路径（不给则写 stdout）")
	dbPath := fs.String("db", "", "本地库路径（默认取环境变量 DATABASE_PATH）")
	tzOffset := fs.Int("tz-offset", store.FixedZoneOffset, "院内本地时区偏移（分钟），小时桶按它切分")
	refresh := fs.Bool("refresh-aggregates", true, "导出前先按区间重算小时级聚合（仅 --kind hourly 用得上）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*from) == "" || strings.TrimSpace(*to) == "" {
		fmt.Fprintln(os.Stderr, "错误：export 需要 --from 与 --to 指定统计区间（RFC3339 带偏移）")
		return exitUsage
	}
	if *kind != "weekly" && *kind != "hourly" {
		fmt.Fprintf(os.Stderr, "错误：--kind 取值不合法：%q（合法取值：weekly、hourly）\n", *kind)
		return exitUsage
	}
	if *tzOffset%60 != 0 {
		fmt.Fprintf(os.Stderr, "错误：--tz-offset 必须是整小时（分钟数能被 60 整除），收到 %d\n", *tzOffset)
		return exitUsage
	}

	fromAt, err := parseRFC3339Arg("--from", *from)
	if err != nil {
		return reportError(err)
	}
	toAt, err := parseRFC3339Arg("--to", *to)
	if err != nil {
		return reportError(err)
	}
	if toAt.Before(fromAt) {
		return reportError(errcode.Newf(errcode.Validation,
			"导出区间无效：起 %s 晚于止 %s",
			fromAt.Format(time.RFC3339), toAt.Format(time.RFC3339)))
	}

	cfg, _ := config.LoadLenient()
	db := cfg.DatabasePath
	if strings.TrimSpace(*dbPath) != "" {
		db = strings.TrimSpace(*dbPath)
	}
	if strings.TrimSpace(db) == "" {
		return reportError(errcode.Newf(errcode.Validation,
			"未指定本地库路径：请用 --db 指定，或设置环境变量 DATABASE_PATH"))
	}

	st, err := store.Open(db)
	if err != nil {
		return reportError(err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	writer, closer, err := csvDestination(*out)
	if err != nil {
		return reportError(err)
	}
	defer func() { _ = closer() }()

	switch *kind {
	case "weekly":
		rows, err := report.BuildWeeklyReport(ctx, st, fromAt, toAt, *point)
		if err != nil {
			return reportError(err)
		}
		if err := report.WriteWeeklyCSV(writer, rows); err != nil {
			return reportError(err)
		}
		summary := report.SummarizeWeekly(rows, fromAt, toAt)
		fmt.Fprintf(os.Stderr, "周报已导出 → %s：%s\n", destinationLabel(*out), summary.String())
	case "hourly":
		if *refresh {
			buckets, err := st.RefreshHourlyAggregates(ctx, *point, fromAt, toAt, *tzOffset)
			if err != nil {
				return reportError(err)
			}
			fmt.Fprintf(os.Stderr, "已按区间重算 %d 个小时级聚合桶\n", buckets)
		}
		aggs, err := st.HourlyAggregates(ctx, *point, fromAt, toAt)
		if err != nil {
			return reportError(err)
		}
		loc := time.FixedZone("冷链本地时区", *tzOffset*60)
		if err := report.WriteHourlyCSV(writer, aggs, loc); err != nil {
			return reportError(err)
		}
		points := make(map[string]bool)
		for _, agg := range aggs {
			points[agg.PointID] = true
		}
		fmt.Fprintf(os.Stderr, "小时级聚合已导出 → %s：%d 个点位、%d 个小时桶\n",
			destinationLabel(*out), len(points), len(aggs))
	}
	return exitOK
}

// runArchive 把过期原始事件归档成 gzip JSONL，并从热表清理。
//
// 多点位 5 分钟粒度持续上报，原始采样一天就几万条，全量堆在热表上，
// 库里迟早撑爆院内那台小主机。冷链管理的规矩是「原始记录要留、但不必一直躺在热库上」：
// 过期采样写成 gzip JSONL（文件名带点位与时间区间），热表只留最近一段，
// 小时级聚合表留住统计口径；需要复盘时把归档文件当取数来源重跑一遍判定。
//
// 顺序固定为「先落盘、后删表」：归档文件写不成功，就绝不删热表里的行。
// 顺序反过来的话，删完表才发现文件没写成，这段原始读数就没了。
//
// stdout 出归档回执 JSON（可被管道消费），人话摘要写 stderr。
// 退出码：0 = 归档完成（本来就无过期数据也算）；1 = 归档失败；2 = 用法错误。
func runArchive(args []string) int {
	fs := flag.NewFlagSet("archive", flag.ContinueOnError)
	before := fs.String("before", "", "归档截止时刻：该时刻及之前的原始采样被归档（RFC3339 带偏移，必填）")
	point := fs.String("point", "", "只归档某个点位（不给则全部点位）")
	dir := fs.String("dir", "", "归档目录（默认取环境变量 ARCHIVE_DIR）")
	dbPath := fs.String("db", "", "本地库路径（默认取环境变量 DATABASE_PATH）")
	tzOffset := fs.Int("tz-offset", store.FixedZoneOffset, "院内本地时区偏移（分钟）")
	dryRun := fs.Bool("dry-run", false, "只统计将被归档的采样条数，不写文件也不清热表")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if strings.TrimSpace(*before) == "" {
		fmt.Fprintln(os.Stderr, "错误：archive 需要 --before <RFC3339> 指定归档截止时刻")
		return exitUsage
	}
	if *tzOffset%60 != 0 {
		fmt.Fprintf(os.Stderr, "错误：--tz-offset 必须是整小时（分钟数能被 60 整除），收到 %d\n", *tzOffset)
		return exitUsage
	}
	beforeAt, err := parseRFC3339Arg("--before", *before)
	if err != nil {
		return reportError(err)
	}

	cfg, _ := config.LoadLenient()
	db := cfg.DatabasePath
	if strings.TrimSpace(*dbPath) != "" {
		db = strings.TrimSpace(*dbPath)
	}
	if strings.TrimSpace(db) == "" {
		return reportError(errcode.Newf(errcode.Validation,
			"未指定本地库路径：请用 --db 指定，或设置环境变量 DATABASE_PATH"))
	}
	archiveDir := cfg.ArchiveDir
	if strings.TrimSpace(*dir) != "" {
		archiveDir = strings.TrimSpace(*dir)
	}
	if strings.TrimSpace(archiveDir) == "" {
		return reportError(errcode.Newf(errcode.Validation,
			"未指定归档目录：请用 --dir 指定，或设置环境变量 ARCHIVE_DIR"))
	}

	st, err := store.Open(db)
	if err != nil {
		return reportError(err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	if *dryRun {
		events, err := st.EventsInRange(ctx, *point, time.Time{}, beforeAt)
		if err != nil {
			return reportError(err)
		}
		fmt.Fprintf(os.Stderr, "干跑：%s 及之前共有 %d 条原始采样会被归档到 %s（本轮不写文件、不清热表）\n",
			beforeAt.Format(time.RFC3339), len(events), archiveDir)
		return exitOK
	}

	infos, err := st.ArchiveEvents(ctx, *point, beforeAt, archiveDir, *tzOffset)
	if err != nil {
		return reportError(err)
	}
	if len(infos) == 0 {
		fmt.Fprintf(os.Stderr, "没有 %s 及之前的原始采样，无需归档\n", beforeAt.Format(time.RFC3339))
		return exitOK
	}

	total := 0
	for _, info := range infos {
		total += info.EventCount
		fmt.Fprintf(os.Stderr, "已归档点位 %s：%d 条采样，%s ~ %s，写入 %s（%d 字节）\n",
			info.PointID, info.EventCount,
			info.From.Format(time.RFC3339), info.To.Format(time.RFC3339),
			info.Path, info.Bytes)
	}
	fmt.Fprintf(os.Stderr, "归档完成：共 %d 个点位、%d 条原始采样移出热表；归档文件可用 coldchain replay 读回复盘\n",
		len(infos), total)
	return exitOK
}

// parseRFC3339Arg 解析一个 RFC3339 时间参数。
//
// 统一在这里报错：命令行给的时间写错时，报错要指名是哪个参数、
// 该长什么样，而不是把 time.Parse 的英文原文甩给使用者。
func parseRFC3339Arg(name, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, errcode.Newf(errcode.Validation,
			"解析 %s 失败：%s（需 RFC3339 带偏移，如 2026-10-19T00:00:00+08:00）", name, value)
	}
	return parsed, nil
}

// csvDestination 打开导出目的地：给了路径就写文件，没给就写 stdout。
//
// 返回的 closer 负责收尾（关文件）；stdout 的 closer 是空操作。
func csvDestination(path string) (io.Writer, func() error, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return os.Stdout, func() error { return nil }, nil
	}
	if dir := filepath.Dir(trimmed); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, errcode.Newf(errcode.StorageError, "创建导出目录失败：%v", err)
		}
	}
	file, err := os.Create(trimmed)
	if err != nil {
		return nil, nil, errcode.Newf(errcode.StorageError, "创建导出文件失败：%v", err)
	}
	return file, file.Close, nil
}

// destinationLabel 说清导出落在哪儿。
func destinationLabel(path string) string {
	if strings.TrimSpace(path) == "" {
		return "stdout"
	}
	return strings.TrimSpace(path)
}

// usage 打印命令行用法。
func usage() {
	fmt.Fprint(os.Stderr, `coldchain-temp-assessment —— 药品与疫苗冷链温控判定服务

用法：
  coldchain serve      启动常驻 HTTP API 服务（产品的常态运行形态）
  coldchain assess     对一条温度事件做判定并打印结论
  coldchain import     批量导入记录仪导出的温度时序
  coldchain replay     离线回放历史事件，重建判定结论
  coldchain draft      打印一条判定的《温度异常情况记录表》正文
  coldchain batches    查冷链台账的在库批次清单（按点位与时刻）
  coldchain export     导出判定周报 CSV（--kind weekly）或小时级聚合 CSV（--kind hourly）
  coldchain archive    把过期原始事件归档成 gzip JSONL 并从热表清理
  coldchain bench      跑离线对照基准
  coldchain version    打印版本号

常用参数：
  coldchain export  --from 2026-10-19T00:00:00+08:00 --to 2026-10-25T23:59:59+08:00 --out 周报.csv
  coldchain archive --before 2026-10-19T00:00:00+08:00 --dir ./data/archive

可用环境变量见 .env.example；serve 缺少出站基址或凭据时启动即报错退出。
`)
}
