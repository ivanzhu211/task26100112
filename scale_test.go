package revsh

import (
	"runtime"
	"testing"
	"time"
)

// 规模测试：累计 100 万条事件、1000 个同时存活进程、每进程 128 个描述符。
// 验证连续处理完成、告警数量符合预期、退出后状态被释放且引擎内存有界。
func TestScale(t *testing.T) {
	const (
		procs     = 1000
		fds       = 128
		rounds    = 1000 // 每进程 1000 条事件，合计 1_000_000 条
		batchSize = 4096
	)

	eng := New()
	var totalAlerts int
	var batch []Event
	seq := uint64(1)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		totalAlerts += len(eng.ApplyBatch(batch))
		batch = batch[:0]
	}
	push := func(ev Event) {
		ev.Seq = seq
		seq++
		batch = append(batch, ev)
		if len(batch) >= batchSize {
			flush()
		}
	}

	start := time.Now()
	for r := 0; r < rounds; r++ {
		for p := 0; p < procs; p++ {
			pid := uint32(1000 + p)
			fd := int32(r % fds)
			switch r % 4 {
			case 0:
				if r == 0 && fd == 0 {
					push(exec(pid, 1, "/bin/sh"))
					break
				}
				push(open(pid, 1, fd, KindSocket, uint64(r)*procs+uint64(p)+1, r%16 == 0))
			case 1:
				push(dup2(pid, 1, fd-1, 0)) // fd-1 是上一轮打开的套接字
			case 2:
				push(dup2(pid, 1, fd-2, 1)) // fd-2 同样是已打开的套接字
			case 3:
				push(closeFd(pid, 1, fd))
			}
		}
	}
	flush()
	if got := seq - 1; got != 1_000_000 {
		t.Fatalf("generated %d events, want 1_000_000", got)
	}
	if n := eng.LiveProcesses(); n != procs {
		t.Fatalf("LiveProcesses = %d, want %d", n, procs)
	}
	if totalAlerts == 0 {
		t.Fatal("expected alerts during scale run")
	}

	runtime.GC()
	var peak runtime.MemStats
	runtime.ReadMemStats(&peak)
	const limit = 256 << 20
	if peak.HeapAlloc > limit {
		t.Fatalf("HeapAlloc = %d MiB, exceeds 256 MiB", peak.HeapAlloc>>20)
	}

	for p := 0; p < procs; p++ {
		push(exit(uint32(1000+p), 1))
	}
	flush()
	if n := eng.LiveProcesses(); n != 0 {
		t.Fatalf("LiveProcesses after exit = %d, want 0", n)
	}
	if len(eng.procs) != 0 {
		t.Fatalf("engine retains %d exited processes", len(eng.procs))
	}
	t.Logf("1M events in %v, alerts=%d, peak HeapAlloc=%d MiB",
		time.Since(start).Round(time.Millisecond), totalAlerts, peak.HeapAlloc>>20)
}
