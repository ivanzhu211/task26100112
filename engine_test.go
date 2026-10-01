package revsh

import (
	"reflect"
	"testing"
)

func open(pid, gen uint32, fd int32, kind ObjectKind, connID uint64, cloexec bool) Event {
	return Event{PID: pid, Gen: gen, Op: OpOpen, FD: fd, Kind: kind, ConnID: connID, Cloexec: cloexec}
}

func dup2(pid, gen uint32, from, to int32) Event {
	return Event{PID: pid, Gen: gen, Op: OpDup2, FD: from, ToFD: to}
}

func closeFd(pid, gen uint32, fd int32) Event {
	return Event{PID: pid, Gen: gen, Op: OpClose, FD: fd}
}

func setCloexec(pid, gen uint32, fd int32, on bool) Event {
	return Event{PID: pid, Gen: gen, Op: OpSetCloexec, FD: fd, Cloexec: on}
}

func fork(pid, gen, childPID, childGen uint32, shared bool) Event {
	return Event{PID: pid, Gen: gen, Op: OpFork, ChildPID: childPID, ChildGen: childGen, Shared: shared}
}

func exec(pid, gen uint32, program string) Event {
	return Event{PID: pid, Gen: gen, Op: OpExec, Program: program}
}

func exit(pid, gen uint32) Event {
	return Event{PID: pid, Gen: gen, Op: OpExit}
}

// withSeq 按切片顺序补齐严格递增序号（从 1 开始）。
func withSeq(evs []Event) []Event {
	for i := range evs {
		evs[i].Seq = uint64(i + 1)
	}
	return evs
}

func runAll(t *testing.T, evs []Event) []Alert {
	t.Helper()
	var got []Alert
	eng := New()
	for _, ev := range evs {
		got = append(got, eng.Apply(ev)...)
	}
	return got
}

func assertAlerts(t *testing.T, got, want []Alert) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alerts mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// 经典反向 Shell：socket  dup2 到 0 和 1 后告警，序号为首次满足的事件。
func TestBasicReverseShell(t *testing.T) {
	evs := withSeq([]Event{
		open(100, 1, 3, KindSocket, 7, false), // 1
		exec(100, 1, "/bin/sh"),               // 2
		dup2(100, 1, 3, 0),                    // 3：仅 fd0，不满足
		dup2(100, 1, 3, 1),                    // 4：fd0+fd1 同一套接字
		dup2(100, 1, 3, 2),                    // 5：已告警，不重复
	})
	assertAlerts(t, runAll(t, evs), []Alert{{PID: 100, Gen: 1, ConnID: 7, Seq: 4}})
}

// fd0 与 fd2 引用同一套接字同样触发；普通文件不触发。
func TestStderrAndFileObject(t *testing.T) {
	evs := withSeq([]Event{
		open(100, 1, 0, KindFile, 0, false),   // 1
		open(100, 1, 1, KindFile, 0, false),   // 2
		exec(100, 1, "/bin/bash"),             // 3：文件对象不触发
		open(100, 1, 3, KindSocket, 9, false), // 4
		dup2(100, 1, 3, 0),                    // 5
		dup2(100, 1, 3, 2),                    // 6：fd0+fd2 触发
	})
	assertAlerts(t, runAll(t, evs), []Alert{{PID: 100, Gen: 1, ConnID: 9, Seq: 6}})
}

// 远端地址相同的不同连接（不同 ConnID）不能触发。
func TestDistinctConnectionsSamePeer(t *testing.T) {
	evs := withSeq([]Event{
		open(100, 1, 0, KindSocket, 11, false), // 1
		open(100, 1, 1, KindSocket, 12, false), // 2：同一远端地址，不同连接
		exec(100, 1, "/bin/sh"),                // 3：不触发
		dup2(100, 1, 0, 1),                     // 4：覆盖为同一连接后触发
	})
	assertAlerts(t, runAll(t, evs), []Alert{{PID: 100, Gen: 1, ConnID: 11, Seq: 4}})
}

// 同号 dup2 完全不变、保留执行时关闭标记；异号 dup2 清除目标标记。
func TestDup2SameFdKeepsCloexec(t *testing.T) {
	evs := withSeq([]Event{
		open(100, 1, 3, KindSocket, 5, true), // 1：cloexec
		dup2(100, 1, 3, 3),                   // 2：同号，不变，标记保留
		exec(100, 1, "/bin/sh"),              // 3：fd3 被 exec 关闭
		dup2(100, 1, 3, 0),                   // 4：fd3 已不存在，无效
		open(100, 1, 4, KindSocket, 6, true), // 5
		dup2(100, 1, 4, 5),                   // 6：异号，目标标记被清除
		exec(100, 1, "/bin/sh"),              // 7：fd4 关闭，fd5 保留
		dup2(100, 1, 5, 0),                   // 8
		dup2(100, 1, 5, 1),                   // 9：触发，连接 6
	})
	assertAlerts(t, runAll(t, evs), []Alert{{PID: 100, Gen: 1, ConnID: 6, Seq: 9}})
}

// 普通 fork：子进程继承副本并参与判断；之后双方表内修改互不影响。
func TestForkIndependentTables(t *testing.T) {
	evs := withSeq([]Event{
		open(100, 1, 3, KindSocket, 7, false), // 1
		exec(100, 1, "/bin/sh"),               // 2
		dup2(100, 1, 3, 0),                    // 3
		dup2(100, 1, 3, 1),                    // 4：父告警
		fork(100, 1, 200, 1, false),           // 5：子继承 shell+套接字，立即告警
		closeFd(200, 1, 0),                    // 6：子关闭 fd0，不影响父
		fork(100, 1, 300, 1, false),           // 7：父表完好，孙进程仍告警
		closeFd(100, 1, 3),                    // 8：父关闭 fd3，不影响子/孙
		dup2(200, 1, 3, 0),                    // 9：子重新满足，但已告警过，不重复
	})
	assertAlerts(t, runAll(t, evs), []Alert{
		{PID: 100, Gen: 1, ConnID: 7, Seq: 4},
		{PID: 200, Gen: 1, ConnID: 7, Seq: 5},
		{PID: 300, Gen: 1, ConnID: 7, Seq: 7},
	})
}

// 共享模式 clone：一方修改立即可见，一次事件可同时触发多个进程告警。
func TestSharedCloneTriggersAllHolders(t *testing.T) {
	evs := withSeq([]Event{
		exec(100, 1, "/bin/sh"),               // 1
		fork(100, 1, 200, 1, true),            // 2：共享表，子继承 shell
		open(100, 1, 3, KindSocket, 7, false), // 3：双方可见
		dup2(200, 1, 3, 0),                    // 4：子修改，父同样可见
		dup2(100, 1, 3, 1),                    // 5：父与子同时满足
	})
	assertAlerts(t, runAll(t, evs), []Alert{
		{PID: 100, Gen: 1, ConnID: 7, Seq: 5},
		{PID: 200, Gen: 1, ConnID: 7, Seq: 5},
	})
}

// exec 先脱离共享表再关闭 cloexec 描述符：不影响仍共享原表的进程。
func TestExecDetachDoesNotHurtSharers(t *testing.T) {
	evs := withSeq([]Event{
		exec(100, 1, "/bin/sh"),              // 1
		fork(100, 1, 200, 1, true),           // 2：A、B 共享表
		open(100, 1, 3, KindSocket, 7, true), // 3：cloexec，双方可见
		exec(100, 1, "/bin/sh"),              // 4：A 脱离并关闭 fd3；B 的 fd3 保留
		dup2(100, 1, 3, 0),                   // 5：A 的 fd3 已关闭，无效
		dup2(200, 1, 3, 0),                   // 6：B 仍可用
		dup2(200, 1, 3, 1),                   // 7：B 告警，A 不误伤
	})
	assertAlerts(t, runAll(t, evs), []Alert{{PID: 200, Gen: 1, ConnID: 7, Seq: 7}})
}

// SetCloexec 可设置/清除单个描述符标记，且共享表上对另一方可见。
func TestSetCloexecSemantics(t *testing.T) {
	evs := withSeq([]Event{
		exec(100, 1, "/bin/sh"),               // 1
		fork(100, 1, 200, 1, true),            // 2
		open(100, 1, 3, KindSocket, 7, false), // 3
		setCloexec(200, 1, 3, true),           // 4：子设置标记
		exec(100, 1, "/bin/sh"),               // 5：父 exec，fd3 被关闭
		dup2(100, 1, 3, 0),                    // 6：无效
		open(200, 1, 4, KindSocket, 8, true),  // 7
		setCloexec(200, 1, 4, false),          // 8：清除标记
		exec(200, 1, "/bin/bash"),             // 9：fd4 保留
		dup2(200, 1, 4, 0),                    // 10
		dup2(200, 1, 4, 1),                    // 11：告警，连接 8
	})
	assertAlerts(t, runAll(t, evs), []Alert{{PID: 200, Gen: 1, ConnID: 8, Seq: 11}})
}

// PID 复用：不同启动代次是不同生存期，各自独立告警。
func TestPIDReuse(t *testing.T) {
	evs := withSeq([]Event{
		open(100, 1, 3, KindSocket, 7, false), // 1
		exec(100, 1, "/bin/sh"),               // 2
		dup2(100, 1, 3, 0),                    // 3
		dup2(100, 1, 3, 1),                    // 4：第一代告警
		exit(100, 1),                          // 5
		open(100, 2, 3, KindSocket, 7, false), // 6：PID 复用，同一连接
		exec(100, 2, "/bin/sh"),               // 7
		dup2(100, 2, 3, 0),                    // 8
		dup2(100, 2, 3, 1),                    // 9：新生存期再次告警
	})
	eng := New()
	var got []Alert
	for _, ev := range evs {
		got = append(got, eng.Apply(ev)...)
	}
	assertAlerts(t, got, []Alert{
		{PID: 100, Gen: 1, ConnID: 7, Seq: 4},
		{PID: 100, Gen: 2, ConnID: 7, Seq: 9},
	})
	if n := eng.LiveProcesses(); n != 1 {
		t.Fatalf("LiveProcesses = %d, want 1", n)
	}
}

// 条件消失后再次满足不重复告警。
func TestNoRealertAfterConditionFlaps(t *testing.T) {
	evs := withSeq([]Event{
		open(100, 1, 3, KindSocket, 7, false), // 1
		exec(100, 1, "/bin/sh"),               // 2
		dup2(100, 1, 3, 0),                    // 3
		dup2(100, 1, 3, 1),                    // 4：告警
		closeFd(100, 1, 1),                    // 5：条件消失
		dup2(100, 1, 3, 1),                    // 6：再次满足，不重复
		open(100, 1, 4, KindSocket, 8, false), // 7：新连接是新的组合
		dup2(100, 1, 4, 0),                    // 8
		dup2(100, 1, 4, 1),                    // 9：新连接告警
	})
	assertAlerts(t, runAll(t, evs), []Alert{
		{PID: 100, Gen: 1, ConnID: 7, Seq: 4},
		{PID: 100, Gen: 1, ConnID: 8, Seq: 9},
	})
}

// 退出只移除该进程，不破坏其他进程仍可用的描述符。
func TestExitKeepsSharedTable(t *testing.T) {
	evs := withSeq([]Event{
		exec(100, 1, "/bin/sh"),               // 1
		fork(100, 1, 200, 1, true),            // 2
		open(100, 1, 3, KindSocket, 7, false), // 3
		exit(100, 1),                          // 4：父退出，表与子不受影响
		dup2(200, 1, 3, 0),                    // 5
		dup2(200, 1, 3, 1),                    // 6：子正常告警
	})
	assertAlerts(t, runAll(t, evs), []Alert{{PID: 200, Gen: 1, ConnID: 7, Seq: 6}})
}

// 同一事件历史任意分批，告警集合与触发序号完全一致。
func TestBatchingInvariance(t *testing.T) {
	evs := withSeq([]Event{
		exec(100, 1, "/bin/sh"),
		fork(100, 1, 200, 1, true),
		open(100, 1, 3, KindSocket, 7, false),
		fork(200, 1, 300, 1, false),
		dup2(200, 1, 3, 0),
		dup2(100, 1, 3, 1),
		exit(200, 1),
		dup2(300, 1, 3, 1),
	})
	want := runAll(t, evs)

	whole := New().ApplyBatch(evs)
	if !reflect.DeepEqual(whole, want) {
		t.Fatalf("single batch mismatch\n got: %+v\nwant: %+v", whole, want)
	}

	eng := New()
	var chunked []Alert
	for i := 0; i < len(evs); i += 3 {
		end := i + 3
		if end > len(evs) {
			end = len(evs)
		}
		chunked = append(chunked, eng.ApplyBatch(evs[i:end])...)
	}
	if !reflect.DeepEqual(chunked, want) {
		t.Fatalf("chunked batch mismatch\n got: %+v\nwant: %+v", chunked, want)
	}
}
