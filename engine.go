package revsh

import "sort"

// object 是描述符引用的底层对象（普通文件或已连接套接字）。
// 不再被任何描述符引用时由 GC 回收，连接在仍有引用时持续有效。
type object struct {
	kind   ObjectKind
	connID uint64
}

// fdEntry 是描述符表项：对象引用 + 执行时关闭标记（标记属于表项而非对象）。
type fdEntry struct {
	obj     *object
	cloexec bool
}

// fdTable 是描述符表；共享模式 clone 的多个进程持有同一指针，
// 一方修改对另一方立即可见。
type fdTable struct {
	fds     map[int32]fdEntry
	holders map[*process]struct{} // 当前持有该表的进程
}

func newTable() *fdTable {
	return &fdTable{fds: make(map[int32]fdEntry), holders: make(map[*process]struct{})}
}

// process 表示一个进程生存期（PID + 启动代次）。
type process struct {
	pid     uint32
	gen     uint32
	program string
	table   *fdTable
	alerted map[uint64]struct{} // 本生存期内已告警的连接标识
}

type procKey struct {
	pid uint32
	gen uint32
}

// Engine 是反向 Shell 检测引擎，状态仅保存在内存，仅支持顺序调用。
type Engine struct {
	procs map[procKey]*process
}

// New 创建一个空引擎。
func New() *Engine {
	return &Engine{procs: make(map[procKey]*process)}
}

// LiveProcesses 返回当前存活进程数，供调用方监控状态规模。
func (e *Engine) LiveProcesses() int {
	return len(e.procs)
}

// Apply 应用单条事件，返回该事件完成后新增的告警。
// 同一事件触发多个告警时按 (PID, Gen, ConnID) 排序，保证输出确定。
func (e *Engine) Apply(ev Event) []Alert {
	var out []Alert
	switch ev.Op {
	case OpOpen:
		p := e.mustProc(ev.PID, ev.Gen)
		p.table.fds[ev.FD] = fdEntry{obj: &object{kind: ev.Kind, connID: ev.ConnID}, cloexec: ev.Cloexec}
		out = e.checkTable(p.table, ev.Seq)
	case OpDup2:
		p := e.mustProc(ev.PID, ev.Gen)
		if ev.FD != ev.ToFD {
			if src, ok := p.table.fds[ev.FD]; ok {
				// 覆盖目标：引用源对象，并清除目标的执行时关闭标记。
				p.table.fds[ev.ToFD] = fdEntry{obj: src.obj}
			}
		}
		// 源与目标相同则完全不变（保留目标原有标记）。
		out = e.checkTable(p.table, ev.Seq)
	case OpClose:
		p := e.mustProc(ev.PID, ev.Gen)
		delete(p.table.fds, ev.FD)
		out = e.checkTable(p.table, ev.Seq)
	case OpSetCloexec:
		p := e.mustProc(ev.PID, ev.Gen)
		if ent, ok := p.table.fds[ev.FD]; ok {
			ent.cloexec = ev.Cloexec
			p.table.fds[ev.FD] = ent
		}
		out = e.checkTable(p.table, ev.Seq)
	case OpFork:
		parent := e.mustProc(ev.PID, ev.Gen)
		child := &process{
			pid:     ev.ChildPID,
			gen:     ev.ChildGen,
			program: parent.program, // 继承父进程的当前程序
			alerted: make(map[uint64]struct{}),
		}
		if ev.Shared {
			child.table = parent.table // 共享同一张描述符表
		} else {
			child.table = newTable()
			for fd, ent := range parent.table.fds { // 独立副本，之后互不影响
				child.table.fds[fd] = ent
			}
		}
		child.table.holders[child] = struct{}{}
		e.procs[procKey{child.pid, child.gen}] = child
		// 父进程状态未变，只需检查新进程（派生出的 Shell 也参与判断）。
		out = e.checkProc(child, ev.Seq)
	case OpExec:
		p := e.mustProc(ev.PID, ev.Gen)
		p.detach()                         // 先脱离共享描述符表
		for fd, ent := range p.table.fds { // 再移除带执行时关闭标记的描述符
			if ent.cloexec {
				delete(p.table.fds, fd)
			}
		}
		p.program = ev.Program // 最后替换当前程序
		out = e.checkProc(p, ev.Seq)
	case OpExit:
		key := procKey{ev.PID, ev.Gen}
		if p, ok := e.procs[key]; ok {
			delete(p.table.holders, p)
			delete(e.procs, key) // 历史告警集合随之释放
		}
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PID != out[j].PID {
			return out[i].PID < out[j].PID
		}
		if out[i].Gen != out[j].Gen {
			return out[i].Gen < out[j].Gen
		}
		return out[i].ConnID < out[j].ConnID
	})
	return out
}

// ApplyBatch 顺序应用一批事件，返回期间全部新增告警。
// 同一事件历史任意分批，告警集合与触发序号完全一致。
func (e *Engine) ApplyBatch(evs []Event) []Alert {
	var out []Alert
	for _, ev := range evs {
		out = append(out, e.Apply(ev)...)
	}
	return out
}

func (e *Engine) mustProc(pid, gen uint32) *process {
	key := procKey{pid, gen}
	if p, ok := e.procs[key]; ok {
		return p
	}
	// 事件历史保证从“新建进程”开始；此处兜底创建初始进程
	// （空描述符表，当前程序不是 Shell）。
	p := &process{pid: pid, gen: gen, table: newTable(), alerted: make(map[uint64]struct{})}
	p.table.holders[p] = struct{}{}
	e.procs[key] = p
	return p
}

// detach 让进程脱离共享描述符表：复制一份内容相同的私有表。
// 之后的修改（含 exec 关闭）不影响原先共享该表的其他进程。
func (p *process) detach() {
	if len(p.table.holders) <= 1 {
		return
	}
	delete(p.table.holders, p)
	nt := newTable()
	for fd, ent := range p.table.fds {
		nt.fds[fd] = ent
	}
	nt.holders[p] = struct{}{}
	p.table = nt
}

// checkTable 检查共享该表的全部进程（共享修改可能同时使多个进程满足条件）。
func (e *Engine) checkTable(t *fdTable, seq uint64) []Alert {
	var out []Alert
	for p := range t.holders {
		out = append(out, e.checkProc(p, seq)...)
	}
	return out
}

var shellPrograms = map[string]bool{"/bin/sh": true, "/bin/bash": true}

// checkProc 在单个进程上执行告警判定：当前程序精确等于 /bin/sh 或
// /bin/bash，且描述符 0 与 1 或 2 中的至少一个引用同一已连接套接字。
// 每个进程生存期与连接标识的组合最多告警一次。
func (e *Engine) checkProc(p *process, seq uint64) []Alert {
	if !shellPrograms[p.program] {
		return nil
	}
	e0, ok := p.table.fds[0]
	if !ok || e0.obj.kind != KindSocket {
		return nil
	}
	for _, fd := range [...]int32{1, 2} {
		if ent, ok := p.table.fds[fd]; ok && ent.obj == e0.obj {
			if _, dup := p.alerted[e0.obj.connID]; dup {
				return nil
			}
			p.alerted[e0.obj.connID] = struct{}{}
			return []Alert{{PID: p.pid, Gen: p.gen, ConnID: e0.obj.connID, Seq: seq}}
		}
	}
	return nil
}
