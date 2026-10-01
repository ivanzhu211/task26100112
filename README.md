# revshell — 反向 Shell 行为检测库

消费已规范化的操作事件（打开、dup2、关闭、设置 cloexec、fork、
共享 clone、exec、退出），在内存中维护进程与描述符状态，检测
把 Shell 标准输入输出接到远端连接的行为，并按批返回新增告警。

## 检测规则

事件处理完成后，只要某进程满足以下条件即告警：

- 当前程序精确等于 `/bin/sh` 或 `/bin/bash`；
- 描述符 0 与描述符 1 或 2 中的至少一个引用**同一个**已连接套接字
  （以连接标识区分，相同远端地址的不同连接不算；普通文件不算）。

每个进程生存期（PID + 启动代次）与连接标识的组合最多告警一次。
告警包含进程身份、连接标识和首次满足条件的事件序号。

## 使用

```go
e := revshell.New()

// 事件来自同一主机，Seq 严格递增，均为成功操作。
// 可任意分批提交，告警集合与触发序号与分批方式无关。
alerts := e.ProcessBatch([]revshell.Event{
    {Seq: 1, Proc: revshell.ProcID{PID: 100, StartGen: 1},
     Kind: revshell.OpExec, Path: "/bin/sh"},
    {Seq: 2, Proc: revshell.ProcID{PID: 100, StartGen: 1},
     Kind: revshell.OpOpen, FD: 3, Obj: revshell.ObjSocket, ConnID: 42},
    {Seq: 3, Proc: revshell.ProcID{PID: 100, StartGen: 1},
     Kind: revshell.OpDup2, SrcFD: 3, DstFD: 0},
    {Seq: 4, Proc: revshell.ProcID{PID: 100, StartGen: 1},
     Kind: revshell.OpDup2, SrcFD: 3, DstFD: 1},
})
// alerts == [{Proc:{100 1} ConnID:42 Seq:4}]
```

要点：

- 进程身份为 `ProcID{PID, StartGen}`，PID 复用时以启动代次区分；
  首次见到的进程自动创建（空描述符表、非 Shell 程序）。
- `OpFork` 产生独立描述符表（引用同一连接对象）；
  `OpCloneShared` 让父子共用同一张表，修改互相可见。
- `OpExec` 先脱离共享表，再关闭带 cloexec 标记的描述符，
  最后替换程序；不影响原先共享该表的其他进程。
- 进程退出后其状态与告警历史立即释放，内存不随历史增长。
- 仅支持顺序调用，不做并发控制、持久化与崩溃恢复。

## 测试

```sh
go test ./...
```

- `engine_test.go`：普通 fork 与共享 clone 在复制/关闭/exec 后的
  差异、共享修改触发其他 Shell 告警、exec 关闭不误伤共享方、
  相同远端的不同连接、同号 dup2 保留关闭标记、PID 复用、
  条件反复满足不重复告警、任意分批结果一致。
- `scale_test.go`：100 万事件、约 1000 个同时存活进程、
  每进程 128 个描述符上限内连续处理；验证告警序号递增、
  退出后状态完全释放、堆内存远低于 256 MiB。
