# 写一个服务

> **读者**：要给这个框架加一个新能力的人 ｜ **读完能做**：写出一个能被中心发现、被调用、被测试的服务

协议细节见 [protocol.md](protocol.md)。可跑的样板是 [examples/echo](../examples/echo)。

## 1. 最短路径

```text
1. 建目录 <服务名>/，main.go（或任意语言入口）+ 测试
2. 启动参数只取三样：-name / -addr / -center
3. service.New(cfg) 拿到骨架，cfg.Provides 里写清对外动作
4. s.Handle("动作名", 处理函数) 注册业务；返回的数据就是响应里的 data
5. s.HandleSetting / HandleSecret / HandleReadOnly 声明设置项
6. s.Start(ctx) —— 注册、心跳、名册、重连、告别都由它负责
7. 给每个动作写测试（含错误路径），用替身中心，不依赖外网
```

Go 之外的语言照此实现即可：`protocol/` 与 `service/` 是便利库，不是准入条件。

## 2. 名片

```go
service.Config{
    Name:        "echo",                  // 注册名：小写短名
    Title:       "回声",                   // 中文短名（面板导航用）
    Listen:      "127.0.0.1:9101",
    Center:      "ws://127.0.0.1:9000/ws",
    Description: "把收到的文本加上前缀返回",
    Provides: []protocol.Provide{
        {Action: "echo", Input: "{text}", Output: "{text}"},
    },
}
```

- `input/output` 是给人看的宽松描述，不是 JSON Schema；精确形状由载荷类型定义。
- **要不要暴露给上层 agent**：`Tool: true` 才暴露。查、算、取、发这类能力才给；改配置、清库、发号施令型代理**默认不给**。
- **长耗时**：`Async: true` + `report_result` 补报，不要让调用方等。

## 3. 设置项

```go
// 普通项：值由设置中心保管；留空/取值含义写在 hint 里
s.HandleSetting("prefix", "echo: ", func(v string) error {
    mu.Lock(); prefix = v; mu.Unlock()   // 校验失败就返回 error，中心会保持旧值
    return nil
}, service.Label("回声前缀"), service.Hint("留空表示不加前缀"))

// 秘密项：值不进中心，只有服务自己持有
s.HandleSecret("token", func(v string) error { token = v; return nil },
    service.Label("访问令牌"))

// 只读项：启动值，面板只展示
s.HandleReadOnly("addr", addr, service.Label("监听地址"))
```

规矩：

- 只声明**真正消费**的项，不做「假旋钮」。
- **值是否合法由服务自己校验**——中心只保管与转发，不懂语义。
- 回调在中心下发的 goroutine 里执行，**不要在里面阻塞**（现有值用锁或原子量换掉即可）。

## 4. 测试

服务自带测试，**不依赖外网**：

1. 起一个替身设置中心（应答注册、回一条名册即可，见 [examples/echo/echo_test.go](../examples/echo/echo_test.go)）；
2. `Start` 真实服务；
3. 用 `protocol.Dial(s.WsURL(), nil)` 直连服务，构造 `jump` 调用，断言返回。

断言建议覆盖：名片声明了动作、正常路径的返回值、**错误路径**（缺字段 / 非法值 / 未知动作）。

构造消息时建议**手写字段名**（当「只读规范的第三方」），而不是复用生产结构体——这样契约漂移会立刻被测试抓住。

## 5. 别做的事

- 别 import 其他服务的包，别直接读写别人的库或文件。
- 别让调用方等你完成长任务（用 `async` + `report_result`）。
- 别把管理 / 调试动作暴露给上层 agent。
- 别为快把能力写死进别人的流程——写死一次，就抽不出来一次。
- 别在响应里塞「已受理」当成「已完成」。

## 6. 与设置中心的最小交互

| 时机 | 动作 | 说明 |
| --- | --- | --- |
| 启动 | `register` | 带名片；中心返回全量名册 |
| 每 15s | `heartbeat` | 间隔由名片里的 `heartbeat_ms` 声明 |
| 退出前 | `heartbeat{status:"bye"}` | 中心立即摘除，不等超时 |
| 想看有哪些服务 | `jump{action:"roster"}` | DNS 式拉取 |
| 报错 | `jump{action:"report"}` | 中心记日志；服务侧自己退避防刷 |
| 读设置 | `jump{action:"list_settings"}` | 中心保管的生效值 |

名册与设置都不经中心转业务流量——服务之间点对点直连。
