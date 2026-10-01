package revshell

import "sort"

// fdEntry 是描述符表中的一个条目。条目为值类型：
// 复制描述符表时条目随之拷贝，底层对象通过指针共享。
type fdEntry struct {
	obj     *connObj
	cloExec bool
}

// connObj 表示底层对象：普通文件或已连接套接字。
// 普通 fork 后多个描述符表可引用同一对象；连接在仍有
// 描述符引用时持续有效，因此被引用的套接字即已连接套接字。
type connObj struct {
	isSocket bool
	connID   uint64
}

// fdTable 是一张描述符表，可被共享 clone 的多个进程共同引用。
// users 反向记录当前使用该表的进程，用于共享修改后定位
// 所有可能受影响的进程。
type fdTable struct {
	fds   map[int32]fdEntry
	users map[*process]struct{}
}

func newFdTable() *fdTable {
	return &fdTable{
		fds:   make(map[int32]fdEntry),
		users: make(map[*process]struct{}),
	}
}

// clone 返回一张内容相同、完全独立的新表（普通 fork / exec 脱离用）。
// 条目被拷贝，底层对象引用保持不变，不产生新连接。
func (t *fdTable) clone() *fdTable {
	nt := newFdTable()
	for fd, e := range t.fds {
		nt.fds[fd] = e
	}
	return nt
}

// process 是一个存活进程的运行时状态。
type process struct {
	id    ProcID
	prog  string
	table *fdTable
}

// attach 让进程使用给定表并登记为使用者。
func (p *process) attach(t *fdTable) {
	p.table = t
	t.users[p] = struct{}{}
}

// detach 让进程离开当前表。
func (p *process) detach() {
	if p.table != nil {
		delete(p.table.users, p)
		p.table = nil
	}
}

// Engine 是反向 Shell 检测引擎，状态只保存在内存中。
// 只支持顺序调用，不考虑并发。
type Engine struct {
	procs map[ProcID]*process
	// alerted 按进程记录已告警的连接标识，用于
	// “每个进程生存期与连接标识组合最多告警一次”。
	// 进程退出时其记录整体删除，已退出进程的历史
	// 不会导致内存持续增长。
	alerted map[ProcID]map[uint64]struct{}
}

// New 创建一个新的检测引擎。
func New() *Engine {
	return &Engine{
		procs:   make(map[ProcID]*process),
		alerted: make(map[ProcID]map[uint64]struct{}),
	}
}

// shellPaths 是触发告警的 Shell 程序路径，需精确相等。
var shellPaths = map[string]struct{}{
	"/bin/sh":   {},
	"/bin/bash": {},
}

// ProcessBatch 顺序处理一批事件，返回本批新增触发的告警。
// 告警按 (Seq, PID, StartGen, ConnID) 排序，保证同一事件历史
// 在任意分批方式下输出完全一致。
func (e *Engine) ProcessBatch(events []Event) []Alert {
	var alerts []Alert
	for i := range events {
		alerts = e.apply(events[i], alerts)
	}
	sort.Slice(alerts, func(i, j int) bool {
		a, b := alerts[i], alerts[j]
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		if a.Proc.PID != b.Proc.PID {
			return a.Proc.PID < b.Proc.PID
		}
		if a.Proc.StartGen != b.Proc.StartGen {
			return a.Proc.StartGen < b.Proc.StartGen
		}
		return a.ConnID < b.ConnID
	})
	return alerts
}

// apply 处理单条事件，事件完成后检查受影响进程并追加新告警。
func (e *Engine) apply(ev Event, alerts []Alert) []Alert {
	switch ev.Kind {
	case OpOpen:
		p := e.getOrCreate(ev.Proc)
		obj := &connObj{isSocket: ev.Obj == ObjSocket, connID: ev.ConnID}
		p.table.fds[ev.FD] = fdEntry{obj: obj, cloExec: ev.CloExec}
		return e.checkTable(ev.Seq, p.table, alerts)

	case OpDup2:
		p := e.getOrCreate(ev.Proc)
		if ev.SrcFD != ev.DstFD {
			if src, ok := p.table.fds[ev.SrcFD]; ok {
				// 覆盖目标并清除目标的执行时关闭标记。
				p.table.fds[ev.DstFD] = fdEntry{obj: src.obj}
			}
		}
		// 源与目标相同则完全不变（包括关闭标记）。
		return e.checkTable(ev.Seq, p.table, alerts)

	case OpClose:
		p := e.getOrCreate(ev.Proc)
		delete(p.table.fds, ev.FD)
		// 关闭只能移除引用，不会产生新告警，无需检查。
		return alerts

	case OpSetCloExec:
		p := e.getOrCreate(ev.Proc)
		if entry, ok := p.table.fds[ev.FD]; ok {
			entry.cloExec = ev.CloExec
			p.table.fds[ev.FD] = entry
		}
		// 标记变化不改变对象引用，不会产生新告警。
		return alerts

	case OpFork, OpCloneShared:
		parent := e.getOrCreate(ev.Proc)
		child := &process{id: ev.Child, prog: parent.prog}
		if ev.Kind == OpFork {
			child.attach(parent.table.clone())
		} else {
			child.attach(parent.table)
		}
		e.procs[child.id] = child
		// 派生出的 Shell 也参与判断。
		return e.checkProc(ev.Seq, child, alerts)

	case OpExec:
		p := e.getOrCreate(ev.Proc)
		// 先脱离共享表：得到独立副本，后续关闭不影响其他进程。
		independent := p.table.clone()
		p.detach()
		p.attach(independent)
		// 再移除所有带执行时关闭标记的描述符。
		for fd, entry := range p.table.fds {
			if entry.cloExec {
				delete(p.table.fds, fd)
			}
		}
		// 最后替换当前程序。
		p.prog = ev.Path
		return e.checkProc(ev.Seq, p, alerts)

	case OpExit:
		p := e.getOrCreate(ev.Proc)
		p.detach()
		delete(e.procs, ev.Proc)
		// 释放该进程的告警历史，退出进程不再占用内存。
		delete(e.alerted, ev.Proc)
		return alerts
	}
	return alerts
}

// getOrCreate 返回进程状态；首次见到的进程按规则创建：
// 描述符表为空，当前程序不是 Shell。
func (e *Engine) getOrCreate(id ProcID) *process {
	if p, ok := e.procs[id]; ok {
		return p
	}
	p := &process{id: id}
	p.attach(newFdTable())
	e.procs[id] = p
	return p
}

// checkTable 检查一张表的所有使用进程（共享修改可能同时
// 使多个进程满足条件，必须全部发现）。
func (e *Engine) checkTable(seq uint64, t *fdTable, alerts []Alert) []Alert {
	for p := range t.users {
		alerts = e.checkProc(seq, p, alerts)
	}
	return alerts
}

// checkProc 在事件完成后判断单个进程是否满足告警条件：
// 当前程序精确等于 /bin/sh 或 /bin/bash，且描述符 0 与
// 描述符 1 或 2 中的至少一个引用同一已连接套接字。
func (e *Engine) checkProc(seq uint64, p *process, alerts []Alert) []Alert {
	if _, ok := shellPaths[p.prog]; !ok {
		return alerts
	}
	in, ok := p.table.fds[0]
	if !ok || !in.obj.isSocket {
		return alerts
	}
	for _, fd := range [...]int32{1, 2} {
		out, ok := p.table.fds[fd]
		if ok && out.obj == in.obj {
			return e.emit(seq, p, in.obj.connID, alerts)
		}
	}
	return alerts
}

// emit 去重后追加告警：每个进程生存期与连接标识的组合最多一次。
func (e *Engine) emit(seq uint64, p *process, connID uint64, alerts []Alert) []Alert {
	seen := e.alerted[p.id]
	if seen == nil {
		seen = make(map[uint64]struct{})
		e.alerted[p.id] = seen
	}
	if _, dup := seen[connID]; dup {
		return alerts
	}
	seen[connID] = struct{}{}
	return append(alerts, Alert{Proc: p.id, ConnID: connID, Seq: seq})
}
