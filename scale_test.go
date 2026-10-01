package revshell

import (
	"runtime"
	"testing"
)

// TestScaleContinuousProcessing 规模测试：
//   - 累计 100 万条事件，分批连续处理；
//   - 同时存活进程约 1000 个，每进程最多 128 个描述符；
//   - 每个进程生存期触发一次告警，验证告警总数与序号单调；
//   - 全部退出后引擎内部状态完全释放，堆内存远低于 256 MiB。
func TestScaleContinuousProcessing(t *testing.T) {
	const (
		totalEvents = 1_000_000
		liveProcs   = 1000
		lifetime    = 1000 // 每个进程生存期内的事件数
		batchSize   = 5000
	)

	e := New()
	var (
		seq        uint64
		connSeq    uint64
		alertCount int
		lastSeq    uint64
	)
	// 每个槽位记录当前进程已推进的事件步数。
	steps := make([]int, liveProcs)
	gens := make([]uint64, liveProcs)
	for i := range gens {
		gens[i] = 1
	}

	next := func() Event {
		seq++
		slot := int(seq % liveProcs)
		p := pid(int32(1000+slot), gens[slot])
		step := steps[slot]
		steps[slot]++
		switch {
		case step == 0:
			// 新生存期开始：变成 shell
			return execTo(seq, p, "/bin/sh")
		case step == 1:
			// 打开已连接套接字（连接标识全局唯一）
			connSeq++
			return openSock(seq, p, 3, connSeq, false)
		case step == 2:
			return dup2(seq, p, 3, 0)
		case step == 3:
			// 触发告警：fd0 与 fd1 引用同一套接字
			return dup2(seq, p, 3, 1)
		case step == lifetime-1:
			// 生存期结束：退出并换代，保证 PID 复用
			steps[slot] = 0
			gens[slot]++
			return exitProc(seq, p)
		default:
			// 生存期中段：混合打开/复制/关闭/标记操作，
			// 描述符在 4..127 之间轮转，不超过 128 个。
			fd := int32(4 + step%124)
			switch step % 4 {
			case 0:
				connSeq++
				return openSock(seq, p, fd, connSeq, step%8 == 0)
			case 1:
				return dup2(seq, p, 3, fd)
			case 2:
				return setClo(seq, p, fd, step%8 == 2)
			default:
				return closeFd(seq, p, fd)
			}
		}
	}

	for processed := 0; processed < totalEvents; processed += batchSize {
		n := batchSize
		if processed+n > totalEvents {
			n = totalEvents - processed
		}
		batch := make([]Event, n)
		for i := range batch {
			batch[i] = next()
		}
		for _, a := range e.ProcessBatch(batch) {
			alertCount++
			if a.Seq <= lastSeq {
				t.Fatalf("告警序号未递增: got %d after %d", a.Seq, lastSeq)
			}
			lastSeq = a.Seq
		}
	}

	if alertCount == 0 {
		t.Fatal("规模测试中未产生任何告警")
	}
	t.Logf("共处理 %d 条事件，产生 %d 条告警", totalEvents, alertCount)

	// 让所有存活进程退出，验证状态完全释放。
	var drain []Event
	for slot := 0; slot < liveProcs; slot++ {
		if steps[slot] == 0 {
			continue // 该槽位上一事件已是退出
		}
		seq++
		drain = append(drain, exitProc(seq, pid(int32(1000+slot), gens[slot])))
	}
	e.ProcessBatch(drain)
	if len(e.procs) != 0 {
		t.Fatalf("退出后仍残留 %d 个进程状态", len(e.procs))
	}
	if len(e.alerted) != 0 {
		t.Fatalf("退出后仍残留 %d 条告警历史", len(e.alerted))
	}

	// 引擎额外内存远低于 256 MiB。
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	const limit = 256 << 20
	if ms.HeapAlloc >= limit {
		t.Fatalf("堆内存超限: %d MiB >= 256 MiB", ms.HeapAlloc>>20)
	}
	t.Logf("全部退出后堆内存 %d MiB", ms.HeapAlloc>>20)
}
