# gorge-webhook

Gorge 平台中的 Herald Webhook 投递微服务，以独立 Go 服务的方式替代 Phorge 内置的 PHP webhook 投递逻辑。

通过直接读取 Phorge 的 MySQL Herald 数据库表，异步轮询并投递 webhook 请求，支持并发控制、HMAC-SHA256 签名、重试策略和错误回退机制。

## 架构

```
Phorge Herald 规则 ──写入──▶ herald_webhookrequest 表
                                      │
                              gorge-webhook 轮询
                                      │
                                      ▼
                            构建 Payload + HMAC 签名
                                      │
                              POST 到目标 URL
```

## 配置

通过环境变量配置，无需配置文件。

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `MYSQL_HOST` | `127.0.0.1` | MySQL 主机地址 |
| `MYSQL_PORT` | `3306` | MySQL 端口 |
| `MYSQL_USER` | `phorge` | MySQL 用户名 |
| `MYSQL_PASS` | (空) | MySQL 密码 |
| `STORAGE_NAMESPACE` | `phorge` | Phorge 数据库名前缀，连接 `{namespace}_herald` 数据库 |
| `LISTEN_ADDR` | `:8160` | HTTP 服务监听地址 |
| `SERVICE_TOKEN` | (空) | API 认证 Token，为空时跳过认证 |
| `POLL_INTERVAL_MS` | `1000` | 轮询间隔（毫秒） |
| `DELIVERY_TIMEOUT` | `15` | 单次 webhook HTTP 请求超时（秒） |
| `MAX_CONCURRENT` | `8` | 最大并发投递数 |
| `ERROR_BACKOFF_SEC` | `300` | 错误回退窗口（秒） |
| `ERROR_THRESHOLD` | `10` | 触发回退的错误次数阈值 |

## API

### GET /

健康检查端点。

### GET /healthz

健康检查端点（Docker HEALTHCHECK 使用）。

### GET /api/webhook/stats

返回投递统计信息。需要 `X-Service-Token` 请求头或 `token` 查询参数认证。

```json
{
  "data": {
    "queuedCount": 5,
    "sentCount": 1200,
    "failedCount": 3,
    "activeWebhooks": 4
  }
}
```

### GET /api/webhook/hooks

返回 webhook 总数。需要认证。

```json
{
  "data": {
    "total": 6
  }
}
```

## 构建 & 运行

```bash
go build -o gorge-webhook ./cmd/server
./gorge-webhook
```

## Docker

```bash
docker build -t gorge-webhook .
docker run -p 8160:8160 \
  -e MYSQL_HOST=db.example.com \
  -e MYSQL_PASS=secret \
  gorge-webhook
```

## 项目结构

```
gorge-webhook/
├── cmd/server/
│   └── main.go                     # 服务入口，启动 Dispatcher 和 HTTP 服务器
├── internal/
│   ├── config/
│   │   └── config.go               # 环境变量配置加载
│   ├── delivery/
│   │   ├── model.go                # 领域模型（WebhookRequest, Webhook, Payload 等）
│   │   ├── store.go                # 数据库访问层（MySQL Herald 表操作）
│   │   └── dispatcher.go           # 后台调度器：轮询、并发投递、HMAC 签名
│   └── httpapi/
│       └── handlers.go             # HTTP 路由与处理器（健康检查、统计接口）
├── Dockerfile                      # 多阶段 Docker 构建
├── go.mod
└── go.sum
```

## 技术栈

- **语言**：Go 1.26
- **HTTP 框架**：[Echo](https://github.com/labstack/echo) v4.15.1
- **数据库驱动**：[go-sql-driver/mysql](https://github.com/go-sql-driver/mysql) v1.9.3
- **许可证**：Apache License 2.0
