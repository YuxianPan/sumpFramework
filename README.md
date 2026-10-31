# SUMP Framework

[![CI](https://github.com/NouSart/sumpFramework/actions/workflows/ci.yml/badge.svg)](https://github.com/NouSart/sumpFramework/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)

> 把「能力」做成可寻址的服务，让它们彼此按地址直连——没有中心交换机，没有进程内共享状态。

这是 [SUMP](https://github.com/NouSart/sump) 抽出来的框架层：**协议**（怎么说话）与**服务骨架**（注册 / 心跳 / 名册 / 重连 / 设置）。参考实现见 `examples/echo`。

## 三条硬规则

1. **扩展不需要修改任何其他服务**——新增或替换服务，不得要求别人改代码。
2. **唯一共享物是协议**——不共享进程内状态，不 import 别人的包，不直接读写别人的库或文件。
3. **能力可替换**——同名同语义的实现，任何语言都能顶替。

> 判断一个改动是否合规的最快问法：**它需要别的服务配合修改吗？** 需要，就是设计走偏了。

## 30 行接进来

```go
s := service.New(service.Config{
    Name:   "echo",
    Listen: "127.0.0.1:9101",
    Center: "ws://127.0.0.1:9000/ws",
    Provides: []protocol.Provide{
        {Action: "echo", Input: "{text}", Output: "{text}"},
    },
})
s.HandleSetting("prefix", "echo: ", func(v string) error { prefix = v; return nil })
s.Handle("echo", func(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
    var p struct{ Text string `msgpack:"text"` }
    if err := protocol.DecodeRaw(in.Input, &p); err != nil {
        return nil, err
    }
    return map[string]string{"text": prefix + p.Text}, nil
})
s.Start(ctx)      // 注册 / 心跳 / 名册 / 重连 / 告别都在里面
```

完整可跑的版本：[examples/echo/main.go](examples/echo/main.go)（含测试）。

服务只写**三样启动参数**：自己的名字、监听地址、设置中心地址。其余全由骨架负责：

```text
启动 → 监听 WS → 向中心 register（带名片）
     → 每 15s heartbeat
     → 订阅 roster（名册变化即替换本地通讯录）
     → 受理 jump → 回 response
     → 退出前发 bye（中心立即摘除）
断线 → 指数退避重连（1s → 30s）+ 重新注册；名片没变不广播
```

## 包里有什么

| 目录 | 内容 |
| --- | --- |
| [`protocol/`](protocol/) | 信封、消息类型、载荷类型、ID、客户端。**协议的参考实现**，不是规范本身 |
| [`service/`](service/) | 服务骨架：注册 / 心跳 / 名册 / 断线自愈 / 优雅下线 / 设置接收 / 秘密项 |
| [`examples/echo/`](examples/echo/) | 最小服务 + 它的测试（替身中心，不依赖外网） |
| [`reasoner/`](reasoner/) | 单步推理服务：千问（OpenAI 兼容）文本补全 + 参考资料拼接（含测试） |
| [`docs/protocol.md`](docs/protocol.md) | 协议契约：信封、动作、`provides`、设置三类、异步补报、链规则 |
| [`docs/add-a-service.md`](docs/add-a-service.md) | 写一个服务：名片、设置项、测试要求、别做的事 |

## 核心概念

| 概念 | 一句话 |
| --- | --- |
| **jump** | 服务之间唯一的「访问」动作。目标可以是自己——自跳就是循环的下一步 |
| **boss** | 本跳「面向谁工作」（结果归属）。逐跳自定，**不可丢失** |
| **provides** | 名片里声明「我提供哪些动作」。`tool: true` 才暴露给上层 agent |
| **async** | 长耗时动作受理即回执，之后用 `report_result` 补报，不让调用方等 |
| **settings** | 三类设置项：普通（中心保管）/ 秘密（谁消费谁持有）/ 只读（启动值） |

## 不是什么

- **不是 agent 框架**：骨架里没有循环、没有模型调用、没有 prompt。上层 agent 自己决定跳哪；模型调用是 [`reasoner/`](reasoner/) 这个**可选服务**，不是框架的一部分。
- **不是消息队列**：没有持久化、没有重投、没有恰好一次。送达后的失败不重试。
- **不是沙箱**：进程隔离靠操作系统，权限边界要自己加。

## 状态

`v0.1`——从 [sump](https://github.com/NouSart/sump) 主线抽出，代码与测试同步自该仓库。
`protocol/` 里同时保留了参考服务用到的载荷类型（文件、批处理、审批等）；框架使用者只需要信封、名片与设置那部分。

## 许可

[Apache-2.0](./LICENSE)。提交 PR 即表示同意以同一许可授权你的贡献。
