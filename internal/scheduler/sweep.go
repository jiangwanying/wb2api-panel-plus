// sweep.go 补领清扫排程：分钟级周期，只 accept 待接受的、只 claim 已达标未领的。
//
// 执行体在面板（panel/tasksweep.go）：main 装配期用 SetSweepRunner 注入
// （panel 依赖本包，反向 import 会成环）。独立于 Run 的小时制排程——补领是分钟级
// 观测量，且不跑行为链路，代价只是每账号 2 次列表请求。
package scheduler

import (
	"context"
	"log"
	"time"
)

// RunTaskSweepNow 立即执行一轮补领清扫。未注入执行器（nil）时静默返回：
// 测试与未装配面板的部署不必额外判断。
func (s *Scheduler) RunTaskSweepNow() {
	s.schedMu.Lock()
	fn := s.cfg.SweepRunner
	s.schedMu.Unlock()
	if fn == nil {
		return
	}
	_, msg := fn()
	log.Printf("补领清扫：%s", msg)
}

// StartTaskSweep 后台周期补领清扫（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.task_sweep_enabled=false 时 main 不调用即可）。
// 运行期可用 SetTaskSweepInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartTaskSweep(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.sweepInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.sweepInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 补领清扫每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmSweep:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmSweep:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunTaskSweepNow()
			}
		}
	}()
}

// SetTaskSweepInterval 热改补领清扫间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetTaskSweepInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.sweepInterval.Store(int64(d))
	poke(s.rearmSweep)
}
