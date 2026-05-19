# aliyun-ecs-monitor

`aliyun-ecs-monitor` 是一个长期运行的 daemon。它轮询阿里云 ECS 实例状态，发现配置里的实例处于 `Stopped` 时就尝试启动，并在需要时发送 Telegram 通知。

它不是 one-shot 工具。没有“检查一次就退出”或“启动一次就退出”的监控模式。`-check-config` 只做配置校验，不会发起阿里云或 Telegram 网络请求。

## 行为概览

- 只处理配置里的实例组和实例 ID。
- 只会把状态为 `Stopped` 的实例当成可启动对象。
- `DescribeInstanceStatus` 只能告诉你当前状态，不能证明停止原因一定是抢占式实例回收或 spot 价格问题。
- 如果实例已经是 `Starting`、`Stopping` 或其他状态，程序会继续观察，不会重复启动。
- Telegram 是可选项。`bot_token` 和 `chat_id` 任何一个为空，通知就自动关闭。
- Telegram 只发事件通知，不做心跳，不做定时摘要。

## 配置文件

默认配置路径是 `config.yaml`。可以用 `-config` 指定其他路径。

### YAML schema

```yaml
access_key_id: string
access_key_secret: string
poll_interval: duration
telegram:
  bot_token: string
  chat_id: string
instances:
  - region_id: string
    instance_ids:
      - string
```

### 配置示例

下面示例和 `config.example.yaml` 一致，值都是假的。

```yaml
access_key_id: test-access-key-id
access_key_secret: test-access-key-secret
poll_interval: 60s
telegram:
  bot_token: ""
  chat_id: ""
instances:
  - region_id: cn-hangzhou
    instance_ids:
      - i-example123
```

### 字段说明

- `access_key_id`, `access_key_secret`，阿里云 AccessKey。
- `poll_interval`，轮询间隔，格式用 Go duration，例如 `60s`。
- `telegram.bot_token`, `telegram.chat_id`，可选。都为空时禁用通知。
- `instances`，实例组列表。
- `instances[].region_id`，区域 ID，例如 `cn-hangzhou`。
- `instances[].instance_ids`，要监控的 ECS 实例 ID 列表。

## 本地运行

先准备好 `config.yaml`，然后直接启动 daemon。

```bash
go run ./cmd/aliyun-ecs-monitor
```

指定配置路径：

```bash
go run ./cmd/aliyun-ecs-monitor -config /path/to/config.yaml
```

只校验配置，不启动 daemon：

```bash
go run ./cmd/aliyun-ecs-monitor -check-config -config config.example.yaml
```

`-check-config` 只检查本地 YAML 和字段规则。它不会连接阿里云，也不会给 Telegram 发消息。

## Docker 运行

容器默认读取 `/config.yaml`。运行时把本地配置挂进去即可。

```bash
docker run --rm \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  ghcr.io/<owner>/<repo>:<tag>
```

如果你用的是本地构建镜像，也可以替换成自己的镜像名。

## GHCR 发布

仓库里如果配置了 GHCR 发布工作流，发布方式通常有两种。

- 推送 `v*` 标签时自动发布，例如 `v1.0.0`。
- 通过 manual dispatch 手动触发，并在 `tag` 输入要发布的版本。

镜像地址格式一般是 `ghcr.io/<owner>/<repo>`。

## 阿里云 RAM 权限

运行账号建议用独立 RAM 用户，按最小权限授予。

高层面上，它至少需要：

- 读取 ECS 实例状态的权限。
- 对配置里的实例发起启动操作的权限。

不要把主账号 AccessKey 放进配置文件，也不要把真实密钥提交到仓库。

## Telegram 行为

Telegram 通知是可选的。

- `bot_token` 和 `chat_id` 都非空时，才会发事件消息。
- 为空时，通知功能自动关闭。
- 只发事件类消息，例如发现停止、发起启动、启动成功、启动失败、确认运行。
- 不会发送轮询日志，也不会发送持续心跳。

## 已知限制

如果某个配置实例当前状态是 `Stopped`，程序会尝试启动它。`DescribeInstanceStatus` 只能确认当前状态，不能单独证明停止原因是 spot 价格变化、回收，还是别的原因。

这意味着 README 和日志都不要把“Stopped”直接写成“已经确认是 spot 原因”。

## 排障

### 配置无效

- 检查 `access_key_id` 和 `access_key_secret` 是否为空。
- 检查 `instances` 是否为空。
- 检查每组 `region_id` 是否为空。
- 检查 `instance_ids` 是否为空。
- 检查 `poll_interval` 是否大于最小值，格式是否是合法 duration。

### 找不到配置文件

- 默认会读 `config.yaml`。
- 没有这个文件时，请显式传 `-config /path/to/config.yaml`。

### 没有 Telegram 消息

- 确认 `telegram.bot_token` 和 `telegram.chat_id` 都填写了。
- 任意一个为空时，通知都会关闭。
- 确认程序真的观察到了状态变化，只有事件才会发消息。

### 阿里云启动失败

- 确认 RAM 用户有读取实例状态和启动实例的权限。
- 确认区域 ID 和实例 ID 正确。
- 确认 AccessKey 没有过期。
- 查看日志里的错误摘要。错误信息会尽量避免泄露敏感内容。

### 不能做 live 验证

- 本仓库的 README 和本地验证不包含真实阿里云账号或真实 Telegram 聊天室。
- `-check-config` 只做离线校验，不能替代真实云端联调。

## 说明

这个仓库的目标是让你能把配置写对、把 daemon 跑起来、把发布流程接好。真实云端行为仍然需要你在自己的阿里云和 Telegram 环境里用真实凭据验证。
