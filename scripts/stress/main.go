// 本目录是并发压测的进程内驱动器：把同一批温度采样并发推进判定调度器，
// 核对「判定条数与投递条数解耦」这条不变量。
//
// 为什么要有它：冷库夜间断电数小时，记录仪会把断电期间的每一条采样都推上来，
// 网关重试还会再推几遍。若每条采样都建一条判定 run，药学部主管面前就是
// 几十条同一场断电的判定记录，台账里也会留下几十条越温。
//
// 压测要回答的正是一句：「同一批采样并发灌 60 次，应该判出几条？」
// 答案必须是 1——一个越温窗口只判一次，后续采样并入既有判定。
//
// 用法：
//
//	go run ./scripts/stress --events <采样批次.json> --profile <温区配置.json> --deliveries 20
//
// 退出码：0 表示不变量成立（判定条数 = 1、窗口纳入采样数 = 批内唯一采样数）；
// 非 0 表示不变量破了，脚本据此判失败。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/chaosforge/coldchain-temp-assessment/internal/domain"
	"github.com/chaosforge/coldchain-temp-assessment/internal/logging"
	"github.com/chaosforge/coldchain-temp-assessment/internal/runtime"
)

func main() {
	eventsPath := flag.String("events", "", "采样批次 JSON 文件（数组）")
	profilePath := flag.String("profile", "", "点位温区配置 JSON 文件")
	deliveries := flag.Int("deliveries", 20, "同一批采样投递几遍")
	verbose := flag.Bool("verbose", false, "打印调度器的结构化日志")
	flag.Parse()

	if *eventsPath == "" || *profilePath == "" {
		fmt.Fprintln(os.Stderr, "用法：stress --events <批次.json> --profile <温区配置.json> [--deliveries N]")
		os.Exit(2)
	}

	events, err := loadEvents(*eventsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取采样批次失败：%v\n", err)
		os.Exit(2)
	}
	profile, err := loadProfile(*profilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取温区配置失败：%v\n", err)
		os.Exit(2)
	}
	if len(events) == 0 {
		fmt.Fprintln(os.Stderr, "采样批次为空，压测无从谈起")
		os.Exit(2)
	}

	// 调度器是判定去抖的落点：同一点位的同一次采样投多少遍，
	// 都只应判出一条 run。
	//
	// 日志默认丢弃：压测要看的是「投了多少、判了几条」这几个数，
	// 每次投递一行结构化日志会把结论淹掉（要看得加 --verbose）。
	logger := logging.New(io.Discard)
	if *verbose {
		logger = logging.New(os.Stderr)
	}
	scheduler := runtime.NewScheduler(logger)

	var (
		created   atomic.Int64
		merged    atomic.Int64
		duplicate atomic.Int64
		closed    atomic.Int64
		failed    atomic.Int64
		wg        sync.WaitGroup
	)

	ctx := context.Background()
	total := *deliveries * len(events)
	for round := 0; round < *deliveries; round++ {
		for i := range events {
			ev := events[i]
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := scheduler.Dispatch(ctx, ev, profile)
				if err != nil {
					failed.Add(1)
					return
				}
				switch result.Outcome {
				case runtime.OutcomeCreated:
					created.Add(1)
				case runtime.OutcomeMerged:
					merged.Add(1)
				case runtime.OutcomeDuplicate:
					duplicate.Add(1)
				case runtime.OutcomeClosed:
					closed.Add(1)
				default:
					failed.Add(1)
				}
			}()
		}
	}
	wg.Wait()

	runCount := scheduler.RunCount()
	fmt.Printf("投递数：%d（%d 遍 × %d 条采样）\n", total, *deliveries, len(events))
	fmt.Printf("判定 run 数：%d\n", runCount)
	fmt.Printf("新建 / 并入 / 重复 / 关闭窗口：%d / %d / %d / %d\n",
		created.Load(), merged.Load(), duplicate.Load(), closed.Load())
	if failed.Load() > 0 {
		fmt.Printf("投递失败：%d\n", failed.Load())
	}
	for _, run := range scheduler.Runs() {
		samples := scheduler.SampleCount(run.RunID)
		fmt.Printf("判定 %s：点位 %s、外部引用 %s、纳入采样 %d 条\n",
			run.RunID, run.PointID, run.ExternalRef, samples)
	}

	// 不变量一：同一段越温只判一次——投递数再大，判定条数恒为 1。
	if runCount != 1 {
		fmt.Fprintf(os.Stderr, "不变量破了：%d 条投递判出了 %d 条判定 run（应为 1 条）\n", total, runCount)
		os.Exit(1)
	}
	// 不变量二：重复投递不改变窗口的采样集合——纳入的仍是批内那几条。
	for _, run := range scheduler.Runs() {
		if got := scheduler.SampleCount(run.RunID); got != len(events) {
			fmt.Fprintf(os.Stderr, "不变量破了：窗口纳入采样 %d 条（应为 %d 条）\n", got, len(events))
			os.Exit(1)
		}
	}
	fmt.Println("压测通过：判定条数与投递条数解耦，重复投递未污染窗口样本")
}

// loadEvents 读采样批次文件。
func loadEvents(path string) ([]domain.TemperatureEvent, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var events []domain.TemperatureEvent
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("解析采样批次失败：%w", err)
	}
	return events, nil
}

// loadProfile 读点位温区配置文件。
func loadProfile(path string) (*domain.PointProfile, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var profile domain.PointProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, fmt.Errorf("解析温区配置失败：%w", err)
	}
	return &profile, nil
}
