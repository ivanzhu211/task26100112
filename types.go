// Package revshell 提供反向 Shell 行为检测能力。
//
// 上层调用方按批提交已规范化的操作事件（同一主机、序号严格递增、
// 均为成功操作），引擎在内存中维护进程与描述符状态，每批处理完
// 返回该批新增触发的告警。
package revshell

// OpKind 表示操作事件类型。
type OpKind int

const (
	// OpOpen 打开操作：占用一个空闲描述符，产生普通文件对象或
	// 已连接套接字对象，并指定该描述符的执行时关闭标记。
	OpOpen OpKind = iota
	// OpDup2 复制操作（dup2 语义）：源与目标不同则覆盖目标，
	// 使其引用源对象，并清除目标的执行时关闭标记；
	// 源与目标相同则完全不变。
	OpDup2
	// OpClose 关闭操作：只移除指定描述符。
	OpClose
	// OpSetCloExec 设置标记操作：设置或清除单个描述符的
	// 执行时关闭标记，该标记不属于底层连接对象。
	OpSetCloExec
	// OpFork 普通 fork：产生独立描述符表，初始内容与父进程相同，
	// 之后表内修改互不影响；仍引用相同的连接对象。
	OpFork
	// OpCloneShared 共享模式 clone：父子使用同一张描述符表，
	// 一方的打开、复制、关闭、设置标记对另一方立即可见。
	OpCloneShared
	// OpExec 执行新程序：先脱离共享描述符表，再移除所有带
	// 执行时关闭标记的描述符，最后替换当前程序。
	OpExec
	// OpExit 退出：只移除该进程。
	OpExit
)

// ObjectKind 表示打开操作产生的对象类型。
type ObjectKind int

const (
	// ObjFile 普通文件对象。
	ObjFile ObjectKind = iota
	// ObjSocket 已连接套接字对象，每个新套接字拥有不复用的连接标识。
	ObjSocket
)

// ProcID 唯一标识一个进程：PID 与启动代次共同确定，PID 允许复用。
type ProcID struct {
	PID      int32
	StartGen uint64
}

// Event 是一条已规范化的操作事件。
// Seq 严格递增；字段按操作类型取用：
//   - Open:     FD, Obj, ConnID（ObjSocket 时有效）, CloExec
//   - Dup2:     SrcFD, DstFD
//   - Close:    FD
//   - SetCloExec: FD, CloExec
//   - Fork/CloneShared: Child（子进程身份）
//   - Exec:     Path（新程序路径）
//   - Exit:     无附加字段
type Event struct {
	Seq     uint64
	Proc    ProcID
	Kind    OpKind
	FD      int32
	SrcFD   int32
	DstFD   int32
	Obj     ObjectKind
	ConnID  uint64
	CloExec bool
	Child   ProcID
	Path    string
}

// Alert 表示一次反向 Shell 告警。
// 每个进程生存期与连接标识的组合最多告警一次。
type Alert struct {
	Proc   ProcID
	ConnID uint64
	Seq    uint64 // 首次满足条件的事件序号
}
