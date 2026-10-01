# task26100112

# revsh — 反向 Shell 行为检测库

消费已规范化的操作事件（打开/复制/关闭/设置标记/派生/执行/退出），
在内存中维护进程、描述符表与连接对象状态，检测把 Shell 标准输入输出
接到远端连接的行为，并返回新增告警。不负责采集，无并发、无持久化。

## 检测规则

每个事件完成后，若某进程当前程序精确等于 `/bin/sh` 或 `/bin/bash`，
且描述符 0 与描述符 1 或 2 中的至少一个引用**同一**已连接套接字，
则产生告警。每个进程生存期（PID + 启动代次）与连接标识的组合最多
告警一次；告警携带首次满足条件的事件序号。

## 使用

```go
eng := revsh.New()

// 逐条或分批喂入事件（Seq 需严格递增）；两种方式结果完全一致。
alerts := eng.Apply(revsh.Event{
    Seq: 1, PID: 100, Gen: 1, Op: revsh.OpOpen,
    FD: 3, Kind: revsh.KindSocket, ConnID: 42,
})
alerts = append(alerts, eng.ApplyBatch(batch)...)
```

关键语义：

- `OpOpen`：占用一个空闲描述符，`Kind` 区分普通文件/已连接套接字，
  `ConnID` 标识连接（同远端地址的不同连接须用不同 ConnID），
  `Cloexec` 为执行时关闭标记。
- `OpDup2`：`FD`→`ToFD`；异号覆盖目标并清除其标记，同号完全不变。
- `OpFork`：`Shared=false` 为普通 fork（独立描述符表副本），
  `Shared=true` 为共享模式 clone（双方共用一张表，修改立即可见）。
- `OpExec`：先脱离共享表，再关闭带标记的描述符，最后替换 `Program`；
  不影响原先共享该表的其他进程。
- `OpExit`：只移除该进程，其历史告警记录随之释放。

## 测试

```sh
go test ./...
```

覆盖：普通 fork 与共享 clone 在复制/关闭/exec 后的差异、共享修改
触发其他 Shell 告警、exec 关闭不误伤共享方、同远端不同连接、同号
dup2 保留标记、PID 复用、条件反复满足不重复告警、任意分批结果一致，
以及 100 万事件 / 1000 存活进程 / 每进程 128 描述符的规模与状态释放。
