package revshell

import (
	"reflect"
	"testing"
)

// ---- 事件构造辅助 ----

func pid(id int32, gen uint64) ProcID { return ProcID{PID: id, StartGen: gen} }

func execTo(seq uint64, p ProcID, path string) Event {
	return Event{Seq: seq, Proc: p, Kind: OpExec, Path: path}
}

func openSock(seq uint64, p ProcID, fd int32, conn uint64, cloExec bool) Event {
	return Event{Seq: seq, Proc: p, Kind: OpOpen, FD: fd, Obj: ObjSocket, ConnID: conn, CloExec: cloExec}
}

func openFile(seq uint64, p ProcID, fd int32, cloExec bool) Event {
	return Event{Seq: seq, Proc: p, Kind: OpOpen, FD: fd, Obj: ObjFile, CloExec: cloExec}
}

func dup2(seq uint64, p ProcID, src, dst int32) Event {
	return Event{Seq: seq, Proc: p, Kind: OpDup2, SrcFD: src, DstFD: dst}
}

func closeFd(seq uint64, p ProcID, fd int32) Event {
	return Event{Seq: seq, Proc: p, Kind: OpClose, FD: fd}
}

func setClo(seq uint64, p ProcID, fd int32, on bool) Event {
	return Event{Seq: seq, Proc: p, Kind: OpSetCloExec, FD: fd, CloExec: on}
}

func forkProc(seq uint64, parent, child ProcID) Event {
	return Event{Seq: seq, Proc: parent, Kind: OpFork, Child: child}
}

func cloneShared(seq uint64, parent, child ProcID) Event {
	return Event{Seq: seq, Proc: parent, Kind: OpCloneShared, Child: child}
}

func exitProc(seq uint64, p ProcID) Event {
	return Event{Seq: seq, Proc: p, Kind: OpExit}
}

func alert(p ProcID, conn, seq uint64) Alert {
	return Alert{Proc: p, ConnID: conn, Seq: seq}
}

func runAll(e *Engine, events []Event) []Alert {
	var all []Alert
	for _, ev := range events {
		all = append(all, e.ProcessBatch([]Event{ev})...)
	}
	return all
}

func assertAlerts(t *testing.T, got, want []Alert) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("告警不匹配\n got: %+v\nwant: %+v", got, want)
	}
}

// TestForkIndependentTable 普通 fork 产生独立描述符表：
// 复制、关闭互不影响，但引用同一连接对象。
func TestForkIndependentTable(t *testing.T) {
	r := pid(1, 1)
	f := pid(2, 1)
	events := []Event{
		execTo(1, r, "/bin/bash"),
		openSock(2, r, 3, 10, false),
		forkProc(3, r, f), // F 继承程序与描述符，但表独立
		dup2(4, r, 3, 0),
		dup2(5, r, 3, 1),  // R 满足条件；F 不应受影响
		closeFd(6, r, 0),  // R 条件消失，与 F 无关
		dup2(7, f, 3, 0),  // F 自己完成重定向
		dup2(8, f, 3, 1),  // F 满足条件（同一连接对象 conn10）
		closeFd(9, f, 3),  // F 关闭自己的 fd3，不影响 R 的 fd3
		dup2(10, r, 3, 0), // R 的 fd3 仍在，条件再次满足但不重复告警
	}
	got := runAll(New(), events)
	want := []Alert{
		alert(r, 10, 5),
		alert(f, 10, 8),
	}
	assertAlerts(t, got, want)
}

// TestCloneSharedTable 共享模式 clone：双方共用一张描述符表，
// 一方修改另一方立即可见；exec 脱离共享表并关闭 cloexec 描述符，
// 不影响仍共享该表的进程。
func TestCloneSharedTable(t *testing.T) {
	p := pid(10, 1)
	c := pid(11, 1)
	events := []Event{
		execTo(1, p, "/bin/sh"),
		openSock(2, p, 3, 20, false),
		openSock(3, p, 4, 21, true), // fd4 带执行时关闭标记
		cloneShared(4, p, c),        // C 继承 /bin/sh 并共享描述符表
		dup2(5, c, 3, 0),            // C 的修改对 P 立即可见
		dup2(6, c, 3, 1),            // P 与 C 同时满足条件，都要告警
		execTo(7, p, "/bin/true"),   // P 脱离共享表，关闭自己的 fd4，换程序
		dup2(8, c, 4, 0),            // C 的 fd4 必须仍然存在（未被 P 的 exec 误伤）
		dup2(9, c, 4, 1),            // C 满足条件（conn21）
		execTo(10, p, "/bin/sh"),    // P 重新变成 shell；fd0/fd1 仍是 conn20，已告警过
		openSock(11, p, 5, 22, false),
		dup2(12, p, 5, 0),
		dup2(13, p, 5, 1), // P 以新连接再次告警
	}
	got := runAll(New(), events)
	want := []Alert{
		alert(p, 20, 6),
		alert(c, 20, 6),
		alert(c, 21, 9),
		alert(p, 22, 13),
	}
	assertAlerts(t, got, want)
}

// TestDistinctConnectionsAndPlainFile 相同远端地址的不同连接
// （不同连接标识）不能触发；普通文件不能触发。
func TestDistinctConnectionsAndPlainFile(t *testing.T) {
	s := pid(20, 1)
	events := []Event{
		execTo(1, s, "/bin/sh"),
		openSock(2, s, 3, 30, false), // 远端 10.0.0.1:4444
		openSock(3, s, 4, 31, false), // 同一远端地址，但连接标识不同
		dup2(4, s, 3, 0),
		dup2(5, s, 4, 1), // fd0=conn30, fd1=conn31：两个不同连接，不告警
		dup2(6, s, 3, 1), // fd0=fd1=conn30：告警
		openFile(7, s, 5, false),
		closeFd(8, s, 0),
		dup2(9, s, 5, 0), // fd0=普通文件：不告警
	}
	got := runAll(New(), events)
	want := []Alert{alert(s, 30, 6)}
	assertAlerts(t, got, want)
}

// TestDup2SameFdKeepsCloExec 同号 dup2 完全不变（保留关闭标记）；
// 异号 dup2 清除目标的关闭标记。
func TestDup2SameFdKeepsCloExec(t *testing.T) {
	a := pid(30, 1)
	b := pid(31, 1)
	events := []Event{
		// A：同号 dup2 保留 cloexec，exec 时 fd0 被关闭，不告警
		openSock(1, a, 3, 40, false),
		dup2(2, a, 3, 0),
		dup2(3, a, 3, 1),
		setClo(4, a, 0, true),
		dup2(5, a, 0, 0),        // 同号：完全不变，标记保留
		execTo(6, a, "/bin/sh"), // fd0 被 cloexec 关闭，条件不成立
		// B：异号 dup2 清除标记，exec 后 fd0 保留，告警
		openSock(7, b, 3, 41, false),
		dup2(8, b, 3, 0),
		dup2(9, b, 3, 1),
		setClo(10, b, 0, true),
		dup2(11, b, 3, 0), // 异号：清除 fd0 的关闭标记
		execTo(12, b, "/bin/sh"),
	}
	got := runAll(New(), events)
	want := []Alert{alert(b, 41, 12)}
	assertAlerts(t, got, want)
}

// TestPIDReuse PID 复用：进程身份由 PID 与启动代次共同确定，
// 新生存期是不同进程，可以再次告警；退出释放历史。
func TestPIDReuse(t *testing.T) {
	e := New()
	first := pid(50, 1)
	second := pid(50, 2) // PID 相同，启动代次不同
	var got []Alert
	got = append(got, e.ProcessBatch([]Event{
		execTo(1, first, "/bin/sh"),
		openSock(2, first, 3, 50, false),
		dup2(3, first, 3, 0),
		dup2(4, first, 3, 1),
	})...)
	got = append(got, e.ProcessBatch([]Event{exitProc(5, first)})...)
	// 退出后历史被释放
	if len(e.procs) != 0 || len(e.alerted) != 0 {
		t.Fatalf("退出后状态未释放: procs=%d alerted=%d", len(e.procs), len(e.alerted))
	}
	got = append(got, e.ProcessBatch([]Event{
		execTo(6, second, "/bin/sh"),
		openSock(7, second, 3, 51, false),
		dup2(8, second, 3, 0),
		dup2(9, second, 3, 1),
	})...)
	want := []Alert{
		alert(first, 50, 4),
		alert(second, 51, 9),
	}
	assertAlerts(t, got, want)
}

// TestNoRepeatAlert 每个进程生存期与连接标识的组合最多告警一次，
// 即使条件消失后再次满足也不重复。
func TestNoRepeatAlert(t *testing.T) {
	s := pid(60, 1)
	events := []Event{
		execTo(1, s, "/bin/sh"),
		openSock(2, s, 3, 60, false),
		dup2(3, s, 3, 0),
		dup2(4, s, 3, 1), // 首次满足：告警
		closeFd(5, s, 1), // 条件消失
		dup2(6, s, 3, 1), // 再次满足：不重复
		closeFd(7, s, 0),
		dup2(8, s, 3, 0), // 再次满足：不重复
	}
	got := runAll(New(), events)
	want := []Alert{alert(s, 60, 4)}
	assertAlerts(t, got, want)
}

// TestBatchSplitEquivalence 同一事件历史采用任意分批方式，
// 告警集合及触发序号必须完全一致。
func TestBatchSplitEquivalence(t *testing.T) {
	p := pid(70, 1)
	c := pid(71, 1)
	events := []Event{
		execTo(1, p, "/bin/bash"),
		openSock(2, p, 3, 70, false),
		cloneShared(3, p, c),
		dup2(4, c, 3, 0),
		dup2(5, c, 3, 1),
		execTo(6, p, "/bin/true"),
		openSock(7, c, 4, 71, false),
		dup2(8, c, 4, 0),
		dup2(9, c, 4, 1),
		exitProc(10, p),
		exitProc(11, c),
	}
	single := New().ProcessBatch(events)
	oneByOne := runAll(New(), events)
	// 不规则分批：1/3/2/剩余
	var uneven []Alert
	e := New()
	cuts := []int{1, 3, 2, len(events)}
	pos := 0
	for _, n := range cuts {
		end := pos + n
		if end > len(events) {
			end = len(events)
		}
		uneven = append(uneven, e.ProcessBatch(events[pos:end])...)
		pos = end
	}
	assertAlerts(t, single, oneByOne)
	assertAlerts(t, single, uneven)
}
