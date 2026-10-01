// Package revsh 实现反向 Shell 行为检测：消费规范化的操作事件，
// 在内存中维护进程/描述符/连接状态，并返回新增告警。
package revsh

// Op 表示规范化操作事件的类型。
type Op int

const (
	OpOpen       Op = iota // 打开：占用一个空闲描述符，产生文件或已连接套接字对象
	OpDup2                 // 复制：dup2 语义，FD 为源，ToFD 为目标
	OpClose                // 关闭：移除指定描述符
	OpSetCloexec           // 设置/清除单个描述符的执行时关闭标记
	OpFork                 // 派生进程：Shared 为 true 表示共享描述符表的 clone
	OpExec                 // 执行：脱离共享表、关闭带标记描述符、替换当前程序
	OpExit                 // 退出：移除进程
)

// ObjectKind 表示打开操作产生的对象类别。
type ObjectKind int

const (
	KindFile   ObjectKind = iota // 普通文件对象
	KindSocket                   // 已连接套接字对象
)

// Event 是一条规范化的操作事件。Seq 在同一主机内严格递增。
type Event struct {
	Seq uint64
	PID uint32
	Gen uint32 // 启动代次，与 PID 共同确定进程身份
	Op  Op

	FD      int32 // Open/Close/SetCloexec 的目标描述符；Dup2 的源描述符
	ToFD    int32 // Dup2 的目标描述符
	Kind    ObjectKind
	ConnID  uint64 // KindSocket 时有效，每个新套接字唯一
	Cloexec bool   // Open 的初始标记 / SetCloexec 的目标值

	ChildPID uint32 // Fork 的子进程身份
	ChildGen uint32
	Shared   bool // Fork 是否共享描述符表（共享模式 clone）

	Program string // Exec 替换后的当前程序
}

// Alert 表示一次新增告警：某进程生存期内某连接首次满足反向 Shell 条件。
type Alert struct {
	PID    uint32
	Gen    uint32
	ConnID uint64
	Seq    uint64 // 首次满足条件的事件序号
}
