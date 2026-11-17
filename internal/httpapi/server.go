// Package httpapi 提供冷链温控判定服务的对外 HTTP 接口。
//
// 本轮交付形态为无界面 HTTP API 服务：所有对外交互走 REST + JSON，
// 并由 api/openapi.yaml（OpenAPI 3.1）描述与契约测试约束。
//
// 本文件是路由骨架与统一响应封装，两条约定贯穿全包：
//   - 成功响应直接给出业务对象（调用方拿到的就是它要的那份数据）；
//   - 失败响应统一为 {error_code, message, details}，调用方按 error_code
//     区分「补齐即可重投」（E_VALIDATION / E_RANGE）与「必须人工处理」
//     （E_UNAUTHORIZED / E_RATE_LIMITED / E_STORAGE_ERROR），
//     不必去解析中文文案。
//
// 入站鉴权在 auth.go，入站事件端点在 events.go。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/errcode"
	"github.com/chaosforge/coldchain-temp-assessment/internal/ingest"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
	"github.com/chaosforge/coldchain-temp-assessment/internal/workflow"
)

// Logger 是本包需要的日志能力子集。
//
// 只声明三档级别，不绑死具体实现：服务用 internal/logging 的结构化日志器，
// 测试可以塞一个丢弃输出的实现。
type Logger interface {
	Info(string, ...any)
	Warn(string, ...any)
	Error(string, ...any)
}

// EventIngester 是入站事件落库的能力边界。
//
// 只声明本包真正用到的那一个方法：接收一条已规范化的温度事件，
// 返回事件标识与是否命中幂等键。限流、事务、幂等键这些细节留在 ingest 包内，
// 不外泄到 HTTP 层——这一层只负责「把请求变成事件、把结果变成回执」。
type EventIngester interface {
	Ingest(ctx context.Context, ev domain.TemperatureEvent) (ingest.IngestResult, error)
}

// TemperatureSeriesImporter 是记录仪导出 CSV 批量导入的能力边界。
//
// 与 EventIngester 分开声明：逐条投递与批量导入是两条入场路径，
// 授权与限流口径不同（批量导入不走同点限流），合并成一个接口会
// 让「本层到底依赖了什么」说不清。
type TemperatureSeriesImporter interface {
	ImportSeries(ctx context.Context, data []byte, dryRun bool) (*ingest.ImportResult, error)
}

// RunWritebacker 触发一次出站写回：写冷链台账、发接种门诊通知。
//
// 它是确认动作之后的下一段流程。确认本身只推进状态、固化载荷；
// 真正把这条越温写进台账、通知到门诊，是这里触发的。
type RunWritebacker interface {
	// Writeback 对一条已确认的判定执行出站写回。
	Writeback(ctx context.Context, runID string) (runtime.WritebackOutcome, error)
}

// Route 描述一个已注册的端点，供契约脚本遍历比对。
type Route struct {
	// Method 是 HTTP 方法。
	Method string
	// Path 是路由路径（形如 /v1/events/coldchain-temperature）。
	Path string
	// Permission 是这个端点声明的权限要求。
	// 免鉴权端点为空——它们只回服务自身状态，不碰业务数据。
	Permission workflow.Permission
	// Public 标记免鉴权端点（健康检查与版本）。
	Public bool
}

// Options 是 Server 的构造参数。
type Options struct {
	// Version 是服务版本号，随健康检查与版本端点返回。
	Version string
	// Logger 是结构化日志器；为 nil 时本包不记日志。
	Logger Logger
	// Ingestor 承接事件落库；为 nil 时入站端点一律拒绝服务。
	Ingestor EventIngester
	// Importer 承接记录仪导出 CSV 的批量导入；为 nil 时导入端点一律拒绝服务。
	Importer TemperatureSeriesImporter
	// IngestToken 是入站 Bearer 凭据，只从环境变量 COLDCHAIN_INGEST_TOKEN 读出后传入。
	//
	// 它对应「记录仪 / 院内边缘网关」这个身份：只可投递温度事件。
	// 其余身份（经办 / 主管 / 只读）走 Credentials。
	IngestToken string
	// Credentials 是带身份的凭据表：令牌命中哪一把，就是哪个身份。
	//
	// 凭据表由装配方从环境变量读出后传入，进程内不落盘、不写日志。
	// 角色闸门（roles.go）据此判断「你是谁」与「你能不能做这件事」。
	Credentials []Credential
	// RunReader 承接判定 run 的查询；为 nil 时 run 查询端点一律拒绝服务。
	RunReader RunReader
	// RunLister 承接判定台账清单查询；为 nil 时清单端点一律拒绝服务。
	RunLister RunLister
	// PointProfiles 承接点位温区配置的读写；为 nil 时温区配置端点一律拒绝服务。
	PointProfiles PointProfileRegistry
	// AuditReader 承接判定 run 的审计链查询；为 nil 时审计端点一律拒绝服务。
	AuditReader AuditReader
	// Confirmer 承接判定 run 的确认与否决；为 nil 时确认端点一律拒绝服务。
	Confirmer RunConfirmer
	// Writebacker 承接确认之后的出站写回；为 nil 时确认端点只推进状态、
	// 不触发出站调用（台账与通知都不会收到请求）。
	Writebacker RunWritebacker
	// Compensator 承接待补偿清单与补偿重放；为 nil 时这两个端点一律拒绝服务。
	//
	// 它是「台账写不进去」这件事的出口：没有它，写回失败就只剩一句日志。
	Compensator WritebackCompensator
	// SummaryReader 承接运行摘要的统计；为 nil 时摘要端点一律拒绝服务。
	//
	// 它只读不写：摘要不推进任何业务状态，也不发任何出站请求。
	SummaryReader SummaryReader
	// MaxBodyBytes 是入站请求体上限（字节）；0 时取默认值 1 MiB。
	MaxBodyBytes int64
	// MaxImportBodyBytes 是批量导入请求体上限（字节）；0 时取默认值 16 MiB。
	MaxImportBodyBytes int64
}

// Server 持有 HTTP 路由与依赖。
type Server struct {
	mux                *http.ServeMux
	routes             []Route
	ver                string
	logger             Logger
	ingestor           EventIngester
	importer           TemperatureSeriesImporter
	runReader          RunReader
	runLister          RunLister
	pointProfiles      PointProfileRegistry
	auditReader        AuditReader
	confirmer          RunConfirmer
	writebacker        RunWritebacker
	compensator        WritebackCompensator
	summaryReader      SummaryReader
	creds              []Credential
	maxBodyBytes       int64
	maxImportBodyBytes int64
}

// New 构造一台已注册端点的 Server。
//
// 端点分两类，鉴权口径不同：
//   - 免鉴权：/healthz 与 /v1/version，只回服务自身状态与版本，
//     不回任何凭据、内网地址或业务数据；
//   - 需鉴权：其余全部端点，走「凭据 + 角色权限」两道闸。
//
// 每个需鉴权的端点都必须声明自己要的权限：声明走 handle，
// 没声明的端点一律按默认拒绝处理（见 roles.go）。
func New(opts Options) *Server {
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	maxImportBody := opts.MaxImportBodyBytes
	if maxImportBody <= 0 {
		maxImportBody = defaultMaxImportBodyBytes
	}

	s := &Server{
		mux:                http.NewServeMux(),
		ver:                opts.Version,
		logger:             opts.Logger,
		ingestor:           opts.Ingestor,
		importer:           opts.Importer,
		runReader:          opts.RunReader,
		runLister:          opts.RunLister,
		pointProfiles:      opts.PointProfiles,
		auditReader:        opts.AuditReader,
		confirmer:          opts.Confirmer,
		writebacker:        opts.Writebacker,
		compensator:        opts.Compensator,
		summaryReader:      opts.SummaryReader,
		creds:              effectiveCredentials(opts),
		maxBodyBytes:       maxBody,
		maxImportBodyBytes: maxImportBody,
	}

	s.handlePublic("GET", "/healthz", s.handleHealthz)
	s.handlePublic("GET", "/v1/version", s.handleVersion)

	// 入站端点包一层 Bearer 中间件：凭据不对的请求进不到业务代码。
	// 它的权限要求是「投递温度事件」——记录仪与边缘网关凭据持有这一项。
	s.protect(workflow.PermEventIngest, "POST", "/v1/events/coldchain-temperature",
		s.handleIngestEvent)

	// 批量导入端点走同一道闸：它是另一条入场路径，不是另一套权限体系。
	s.protect(workflow.PermEventIngest, "POST", "/v1/imports/temperature-series",
		s.handleImportSeries)

	// 判定 run 查询端点要的是「查看判定结论」权限：经办、主管与只读岗都持有，
	// 投递通道（记录仪）不持有——它只看温度，不看结论。
	s.protect(workflow.PermRunRead, "GET", "/v1/runs/{id}", s.handleGetRun)

	// 判定台账清单端点回答「这周这个点位判过几段越温」：
	// 与 run 详情端点同一把钥匙，只是把「一条」换成「一段时间内的一批」。
	s.protect(workflow.PermRunRead, "GET", "/v1/runs", s.handleListRuns)

	// 点位温区配置查询端点回答「这台冷柜现在按什么温区判」：
	// 只读，调阅岗也要能看——复核越温结论时这是第一个问题。
	s.protect(workflow.PermRunRead, "GET", "/v1/points", s.handleListPointProfiles)

	// 点位温区配置更新端点是整轮里少数会改业务状态的动作之一：
	// 它改的是判定口径，所以要「更新点位温区配置」这一项权限（经办与主管持有），
	// 调阅岗只看不改，投递通道两者都不持有。
	s.protect(workflow.PermProfileWrite, "PUT", "/v1/points/{point_id}/profile",
		s.handleUpdatePointProfile)

	// 时间线端点回答「哪一步慢、哪一步重试过、现在停在哪」：
	// 与 run 详情端点同一把钥匙，同一份库里的真相，只是换一种看法。
	s.protect(workflow.PermRunRead, "GET", "/v1/runs/{id}/timeline", s.handleGetRunTimeline)

	// 审计链端点回答「这条判定当时用了模型吗、花了多少、谁确认的」：
	// 与 run 详情端点同一把钥匙，同一份库里的真相，只是换一种看法。
	// 只读端点：它连内部状态的边都碰不到。
	s.protect(workflow.PermRunRead, "GET", "/v1/runs/{id}/audit", s.handleGetRunAudit)

	// 确认 / 否决端点是整条链上唯一由人推进业务状态的入口：
	// 它要的是「确认或否决处置结论」这一项权限，只有药学部主管持有。
	// 确认本身不出站——台账写入是另一段流程的事，且必须先过
	// 「未确认不得写回」那道闸门。
	s.protect(workflow.PermConfirm, "POST", "/v1/runs/{id}/confirm", s.handleConfirmRun)
	s.protect(workflow.PermConfirm, "POST", "/v1/runs/{id}/reject", s.handleRejectRun)

	// 待补偿清单端点回答「哪些越温还没进台账」：
	// 它是只读视图，要的是「查看判定结论」这一项权限——
	// 经办、主管与只读岗都持有，投递通道不持有。
	s.protect(workflow.PermRunRead, "GET", "/v1/writebacks/pending", s.handleGetPendingWritebacks)

	// 补偿重放端点回答「把这条补上」：
	// 它要的是「触发台账写回补偿重放」这一项权限（经办与主管持有）。
	// 它不是确认：载荷与幂等键都是上次确认时定下的那份，重放不改内容。
	s.protect(workflow.PermWritebackRetry, "POST", "/v1/runs/{id}/writeback/retry",
		s.handleRetryWriteback)

	// 运行摘要端点回答「现在有事吗、哪一步慢、还有多少条没进台账」：
	// 与 run 详情端点同一把钥匙，同一份库里的真相，只是换一种看法。
	// 它是只读的——摘要不推进任何业务状态，也不发任何出站请求。
	s.protect(workflow.PermRunRead, "GET", "/v1/summary", s.handleSummary)

	return s
}

// effectiveCredentials 汇总出本服务实际生效的凭据表。
//
// 入站凭据（COLDCHAIN_INGEST_TOKEN）固定对应「记录仪 / 边缘网关」身份；
// 凭据表里若已登记同一身份的令牌，以凭据表为准，不重复登记。
func effectiveCredentials(opts Options) []Credential {
	out := make([]Credential, 0, len(opts.Credentials)+1)
	seen := make(map[workflow.Role]bool, len(opts.Credentials)+1)
	for _, cred := range opts.Credentials {
		if cred.Token == "" {
			continue
		}
		if seen[cred.Role] {
			continue
		}
		seen[cred.Role] = true
		out = append(out, cred)
	}
	if opts.IngestToken != "" && !seen[workflow.RoleIngest] {
		out = append(out, Credential{Role: workflow.RoleIngest, Token: opts.IngestToken})
	}
	return out
}

// protect 注册一个需鉴权的端点：先验凭据，再验该端点声明的权限。
//
// 两道闸的顺序不能反：先验凭据才知道「你是谁」，才知道能不能做这件事。
// 中间件是最外层先跑，所以凭据闸套在权限闸外面。
func (s *Server) protect(perm workflow.Permission, method, path string, h http.HandlerFunc) {
	gated := RequirePermission(perm, s.logger)(h)
	authed := BearerAuthAny(s.creds, s.logger)(gated)
	s.handle(method, path, perm, authed.ServeHTTP)
}

// handle 注册一个端点并把它记进路由表。
func (s *Server) handle(method, path string, perm workflow.Permission, h http.HandlerFunc) {
	s.mux.HandleFunc(method+" "+path, h)
	s.routes = append(s.routes, Route{Method: method, Path: path, Permission: perm})
}

// handlePublic 注册一个免鉴权端点。
//
// 免鉴权只给健康检查与版本这类「只回服务自身状态、不碰业务数据」的端点；
// 它们没有权限要求，也不接受任何凭据。
func (s *Server) handlePublic(method, path string, h http.HandlerFunc) {
	s.mux.HandleFunc(method+" "+path, h)
	s.routes = append(s.routes, Route{Method: method, Path: path, Public: true})
}

// Routes 返回本服务已注册的端点清单。
//
// 契约脚本据此逐条比对 api/openapi.yaml：文档里写了、代码里没有的端点，
// 或者代码里有、文档里没写的端点，都会在契约测试里暴露出来。
func (s *Server) Routes() []Route {
	out := make([]Route, len(s.routes))
	copy(out, s.routes)
	return out
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleHealthz 返回服务存活状态，不回任何凭据或内网地址。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "coldchain-temp-assessment",
		"version": s.ver,
	})
}

// handleVersion 返回服务名称与版本。
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "coldchain-temp-assessment",
		"version": s.ver,
	})
}

// writeJSON 写出一个 JSON 响应体。
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError 按错误码写出统一的错误响应体 {error_code, message, details}。
func writeError(w http.ResponseWriter, err *errcode.Error) {
	writeJSON(w, errcode.HTTPStatus(err.Code), err)
}
