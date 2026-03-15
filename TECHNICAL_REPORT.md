# gorge-webhook 技术报告

## 1. 概述

gorge-webhook 是 Gorge 平台中的 Herald Webhook 投递微服务，为 Phorge（Phabricator 社区维护分支）提供可靠的异步 webhook 投递能力。

该服务的核心目标是替代 Phorge 内置的 PHP webhook 投递逻辑。Phorge 的 Herald 规则引擎支持在对象（代码审查、任务、提交等）发生变更时触发 webhook 通知，但原始的投递机制嵌入在 PHP 请求处理流程中，受限于 PHP 的执行模型。gorge-webhook 以独立 Go 服务的方式运行，通过直接读取 Phorge 的 MySQL Herald 数据库表，实现高并发、可重试、带错误回退的 webhook 异步投递。

## 2. 设计动机

### 2.1 原有方案的问题

Phorge 的 Herald webhook 投递机制存在以下局限：

1. **同步阻塞**：webhook 投递嵌入在 PHP 请求处理流程中。当用户在 Phorge 界面执行操作（提交代码审查、更新任务等）触发 Herald 规则时，webhook 的 HTTP 请求在同一 PHP 进程中发出。如果目标服务响应缓慢或不可达，会直接阻塞用户的请求，导致页面加载超时。
2. **并发受限**：PHP 的传统模型是每请求一进程/线程，webhook 投递的并发度受限于 PHP-FPM 的 worker 数量。高频触发场景下，webhook 投递会占用 PHP worker，影响正常的 Web 请求处理。
3. **重试能力弱**：PHP 请求的生命周期有限，无法方便地实现复杂的重试策略。失败的 webhook 请求需要外部机制（如 cron 定时任务）来重试，增加运维复杂度。
4. **缺少回退保护**：当目标服务持续故障时，Phorge 没有内建的回退（backoff）机制来停止对该目标的无效重试，可能导致日志膨胀和资源浪费。

### 2.2 gorge-webhook 的解决思路

以独立 Go 服务重新实现 webhook 投递：

- **异步解耦**：Phorge 只负责将 webhook 请求写入数据库（`herald_webhookrequest` 表），投递工作完全由 gorge-webhook 异步完成。Phorge 端的用户操作不再受 webhook 投递延迟的影响。
- **高并发投递**：利用 Go 的 goroutine 模型，轻松支持多个 webhook 的并行投递，同时通过信号量控制并发上限，避免对目标服务造成过大压力。
- **灵活重试**：根据 Phorge 写入的重试策略（`retry=forever` 或 `retry=never`），对失败的请求进行持续重试或直接放弃。重试天然内嵌在轮询机制中——失败的请求保持 `queued` 状态，下次轮询时自动重新处理。
- **错误回退**：内建滑动窗口的错误计数机制，当某个 webhook 在指定时间窗口内累计失败达到阈值时，自动暂停对该 webhook 的投递，保护系统和目标服务。
- **数据库集成**：不修改 Phorge 代码，通过 MySQL 共享数据库表实现无侵入集成。gorge-webhook 读取 Phorge 写入的数据，更新投递结果，两者通过数据库表作为契约接口。

## 3. 系统架构

### 3.1 在 Gorge 平台中的位置

```
┌──────────────────────────────────────────────────────────────┐
│                        Gorge 平台                            │
│                                                              │
│  ┌──────────┐     写入 webhookrequest 表                     │
│  │  Phorge  │──────────────────────┐                         │
│  │  (PHP)   │                      │                         │
│  └──────────┘                      ▼                         │
│                         ┌─────────────────────┐              │
│                         │   MySQL Database    │              │
│                         │                     │              │
│                         │  herald_webhook     │              │
│                         │  herald_webhook-    │              │
│                         │    request          │              │
│                         └──────────┬──────────┘              │
│                                    │                         │
│                               轮询读取                       │
│                                    │                         │
│                                    ▼                         │
│                         ┌─────────────────────┐              │
│                         │   gorge-webhook     │              │
│                         │                     │              │
│                         │  Dispatcher :8160   │              │
│                         │    后台轮询投递      │              │
│                         │                     │              │
│                         │  HTTP API :8160     │              │
│                         │    GET /healthz     │              │
│                         │    GET /api/stats   │              │
│                         │    GET /api/hooks   │              │
│                         └──────────┬──────────┘              │
│                                    │                         │
│                          POST (HMAC-SHA256 签名)             │
│                                    │                         │
│                                    ▼                         │
│                         ┌─────────────────────┐              │
│                         │   外部 Webhook      │              │
│                         │   目标服务          │              │
│                         └─────────────────────┘              │
└──────────────────────────────────────────────────────────────┘
```

### 3.2 单服务器模型

gorge-webhook 运行一个 HTTP 服务器（默认 `:8160`），同时承载两个职责：

**后台 Dispatcher** 在独立 goroutine 中运行，按固定间隔轮询 MySQL 数据库中 `status=queued` 的 webhook 请求，取出后并发投递到各目标 URL。Dispatcher 是服务的核心引擎，持续运行直到进程收到终止信号。

**HTTP API** 提供健康检查和统计查询端点，供容器编排系统（Docker HEALTHCHECK）和运维监控使用。API 层不参与 webhook 的投递逻辑，仅提供只读的状态查询。

### 3.3 模块划分

项目采用 Go 标准布局，分为三个内部模块：

| 模块 | 路径 | 职责 |
|------|------|------|
| config | `internal/config/` | 环境变量配置加载 |
| delivery | `internal/delivery/` | 核心投递逻辑：数据模型、数据库访问、调度引擎 |
| httpapi | `internal/httpapi/` | HTTP 路由、认证中间件、统计与健康检查接口 |

入口程序 `cmd/server/main.go` 负责串联三个模块：加载配置 → 创建 Store（数据库连接） → 创建 Dispatcher → 启动后台轮询 → 注册 HTTP 路由 → 监听信号优雅退出。

### 3.4 数据库交互模型

gorge-webhook 与 Phorge 通过两张 MySQL 表进行数据交互：

```
┌─────────────────────────────────────────────────────────────┐
│                  {namespace}_herald 数据库                   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │ herald_webhookrequest                                │   │
│  │                                                      │   │
│  │  id | phid | webhookPHID | objectPHID | status       │   │
│  │  properties | lastRequestResult | lastRequestEpoch  │   │
│  │  dateCreated | dateModified                          │   │
│  │                                                      │   │
│  │  Phorge 写入 → gorge-webhook 读取并更新              │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │ herald_webhook                                       │   │
│  │                                                      │   │
│  │  id | phid | name | webhookURI | status | hmacKey    │   │
│  │                                                      │   │
│  │  Phorge 管理 → gorge-webhook 只读查询                │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

**`herald_webhookrequest`** 是请求队列表。Phorge 在 Herald 规则触发时向该表插入 `status=queued` 的记录，gorge-webhook 轮询取出后投递，并将结果（成功/失败）回写到同一行。

**`herald_webhook`** 是 webhook 配置表。由 Phorge 管理（用户在 Herald 界面配置），gorge-webhook 在投递时查询目标 URL 和 HMAC 密钥，只读不写。

## 4. 核心实现分析

### 4.1 配置模块

配置模块位于 `internal/config/config.go`，通过环境变量加载所有运行参数。

#### 4.1.1 数据结构

```go
type Config struct {
    MySQLHost    string
    MySQLPort    int
    MySQLUser    string
    MySQLPass    string
    Namespace    string
    ListenAddr   string
    ServiceToken string

    PollIntervalMs  int
    DeliveryTimeout int
    MaxConcurrent   int
    ErrorBackoffSec int
    ErrorThreshold  int
}
```

配置参数分为三类：

**数据库连接**（`MySQLHost`/`Port`/`User`/`Pass`/`Namespace`）：定义如何连接 Phorge 的 Herald 数据库。`Namespace` 对应 Phorge 的 `STORAGE_NAMESPACE` 配置，用于拼接数据库名 `{namespace}_herald`。

**服务配置**（`ListenAddr`/`ServiceToken`）：HTTP 服务的监听地址和 API 认证令牌。

**投递行为**（`PollIntervalMs`/`DeliveryTimeout`/`MaxConcurrent`/`ErrorBackoffSec`/`ErrorThreshold`）：控制轮询频率、HTTP 超时、并发上限和错误回退策略。这些参数允许根据部署环境和目标服务的承受能力进行调优。

#### 4.1.2 DSN 生成

```go
func (c *Config) HeraldDSN() string {
    return fmt.Sprintf(
        "%s:%s@tcp(%s:%d)/%s_herald?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
        c.MySQLUser, c.MySQLPass, c.MySQLHost, c.MySQLPort, c.Namespace,
    )
}
```

`HeraldDSN()` 生成 MySQL 连接字符串，关键设计：

- **数据库名拼接**：`{namespace}_herald`，与 Phorge 的数据库命名约定一致。Phorge 将不同功能的数据存储在以命名空间为前缀的独立数据库中（如 `phorge_herald`、`phorge_user`），gorge-webhook 只连接 Herald 相关的数据库。
- **超时参数**：`timeout=5s`（连接超时）、`readTimeout=30s`（查询读取超时）、`writeTimeout=30s`（写入超时）。连接超时较短以快速检测数据库不可达，读写超时较长以容纳可能的慢查询。
- **`parseTime=true`**：启用 MySQL 时间类型到 Go `time.Time` 的自动转换。

#### 4.1.3 环境变量辅助函数

```go
func envStr(key, fallback string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return fallback
}

func envInt(key string, fallback int) int {
    if v := os.Getenv(key); v != "" {
        n, err := strconv.Atoi(v)
        if err == nil {
            return n
        }
    }
    return fallback
}
```

两个辅助函数封装了"环境变量优先，不存在或无效时使用默认值"的逻辑。`envInt` 在解析失败时静默回退到默认值，避免因环境变量配置错误导致服务无法启动——在容器化部署中，这种容错行为优于 panic。

### 4.2 数据模型

数据模型位于 `internal/delivery/model.go`，完全映射 Phorge Herald 数据库中的表结构。

#### 4.2.1 常量定义

```go
const (
    RetryNever   = "never"
    RetryForever = "forever"

    StatusQueued = "queued"
    StatusFailed = "failed"
    StatusSent   = "sent"

    ResultNone = "none"
    ResultOkay = "okay"
    ResultFail = "fail"

    ErrorTypeHook    = "hook"
    ErrorTypeHTTP    = "http"
    ErrorTypeTimeout = "timeout"
)
```

常量分为四组：

**重试策略**：`RetryNever`（失败后不重试，直接标记 failed）和 `RetryForever`（失败后保持 queued，等待下次轮询重试）。这两个值由 Phorge 在创建 webhook 请求时设置，gorge-webhook 据此决定失败处理方式。

**请求状态**：三态流转——`queued`（等待投递）→ `sent`（投递成功）或 `failed`（永久失败）。`retry=forever` 的请求在临时失败时保持 `queued` 而非进入 `failed`。

**投递结果**：`none`（未尝试投递）、`okay`（投递成功，2xx 响应）、`fail`（投递失败）。记录在 `lastRequestResult` 字段，与状态字段配合使用。

**错误类型**：`hook`（webhook 配置层面的错误，如 webhook 不存在或被禁用）、`http`（HTTP 层面的错误，如非 2xx 响应或网络失败）、`timeout`（请求超时）。错误类型帮助运维人员快速定位问题根源。

#### 4.2.2 WebhookRequest

```go
type WebhookRequest struct {
    ID                int64  `json:"id"`
    PHID              string `json:"phid"`
    WebhookPHID       string `json:"webhookPHID"`
    ObjectPHID        string `json:"objectPHID"`
    Status            string `json:"status"`
    Properties        string `json:"properties"`
    LastRequestResult string `json:"lastRequestResult"`
    LastRequestEpoch  int64  `json:"lastRequestEpoch"`
    DateCreated       int64  `json:"dateCreated"`
    DateModified      int64  `json:"dateModified"`
}
```

`WebhookRequest` 对应 `herald_webhookrequest` 表的一行记录：

- `PHID`：Phorge 全局唯一标识符，格式为 `PHID-{TYPE}-{hash}`。
- `WebhookPHID`：关联的 webhook 配置 PHID，通过此字段查找目标 URL 和 HMAC 密钥。
- `ObjectPHID`：触发 Herald 规则的对象 PHID（如某个任务或代码审查的 PHID）。
- `Properties`：JSON 字符串，包含重试策略、关联事务、触发器等详细信息。数据库中以文本形式存储，投递时解析为 `WebhookRequestProperties`。

#### 4.2.3 WebhookRequestProperties

```go
type WebhookRequestProperties struct {
    Retry            string   `json:"retry,omitempty"`
    ErrorType        string   `json:"errorType,omitempty"`
    ErrorCode        string   `json:"errorCode,omitempty"`
    TransactionPHIDs []string `json:"transactionPHIDs,omitempty"`
    TriggerPHIDs     []string `json:"triggerPHIDs,omitempty"`
    Silent           bool     `json:"silent,omitempty"`
    Test             bool     `json:"test,omitempty"`
    Secure           bool     `json:"secure,omitempty"`
}
```

`Properties` 字段是一个嵌套的 JSON 结构，承载了每个请求的元数据：

- `Retry`：重试策略，决定失败后的处理方式。
- `ErrorType`/`ErrorCode`：gorge-webhook 在投递失败时回写的错误信息，供 Phorge 界面展示。
- `TransactionPHIDs`：触发此次 webhook 的事务列表（如对某个任务的评论事务）。
- `TriggerPHIDs`：触发此次 webhook 的 Herald 触发器列表。
- `Silent`/`Test`/`Secure`：投递元信息标志，包含在发送给目标服务的 payload 中。

#### 4.2.4 Webhook 配置

```go
type Webhook struct {
    ID         int64  `json:"id"`
    PHID       string `json:"phid"`
    Name       string `json:"name"`
    WebhookURI string `json:"webhookURI"`
    Status     string `json:"status"`
    HmacKey    string `json:"hmacKey"`
}
```

`Webhook` 对应 `herald_webhook` 表，存储 webhook 的目标配置：

- `WebhookURI`：目标服务的 URL，gorge-webhook 将 POST 请求发送到此地址。
- `HmacKey`：HMAC-SHA256 签名密钥。gorge-webhook 使用此密钥对 payload 签名，目标服务可用相同密钥验证请求真实性。
- `Status`：webhook 的启用状态。`disabled` 状态的 webhook 请求会被直接标记为失败，不尝试投递。

#### 4.2.5 投递 Payload

```go
type WebhookPayload struct {
    Object       PayloadObject        `json:"object"`
    Triggers     []PayloadTrigger     `json:"triggers"`
    Action       PayloadAction        `json:"action"`
    Transactions []PayloadTransaction `json:"transactions"`
}

type PayloadObject struct {
    Type string `json:"type"`
    PHID string `json:"phid"`
}

type PayloadAction struct {
    Test   bool  `json:"test"`
    Silent bool  `json:"silent"`
    Secure bool  `json:"secure"`
    Epoch  int64 `json:"epoch"`
}
```

`WebhookPayload` 是发送给目标服务的 JSON 结构，包含：

- `Object`：触发事件的对象信息（类型和 PHID）。`Type` 从 PHID 中提取（如 `PHID-TASK-xxx` 中的 `TASK`）。
- `Triggers`：触发此次 webhook 的 Herald 触发器列表。
- `Action`：投递元信息，包含测试标志、静默标志、安全标志和创建时间戳。
- `Transactions`：关联的事务列表，目标服务可通过 Conduit API 查询事务详情。

### 4.3 数据访问层（Store）

Store 位于 `internal/delivery/store.go`，封装了对 Phorge Herald 数据库的所有操作。

#### 4.3.1 连接管理

```go
type Store struct {
    db              *sql.DB
    errorBackoffSec int
    errorThreshold  int
}

func NewStore(dsn string, errorBackoffSec, errorThreshold int) (*Store, error) {
    db, err := sql.Open("mysql", dsn)
    if err != nil {
        return nil, fmt.Errorf("open db: %w", err)
    }
    db.SetMaxOpenConns(20)
    db.SetMaxIdleConns(5)
    db.SetConnMaxLifetime(5 * time.Minute)

    if err := db.Ping(); err != nil {
        return nil, fmt.Errorf("ping db: %w", err)
    }
    return &Store{
        db:              db,
        errorBackoffSec: errorBackoffSec,
        errorThreshold:  errorThreshold,
    }, nil
}
```

连接池参数设计：

- **`MaxOpenConns=20`**：最大打开连接数。Dispatcher 最多并发 `maxConcurrent`（默认 8）个投递，每个投递涉及 2-3 次数据库查询（取请求、查 webhook、更新结果），加上 HTTP API 的统计查询，20 个连接提供了充足的余量。
- **`MaxIdleConns=5`**：最大空闲连接数。空闲时不保留过多连接，减少 MySQL 端的连接资源占用。
- **`ConnMaxLifetime=5min`**：连接最大生存时间。定期回收连接，防止长连接导致的 MySQL 端资源泄漏或状态异常。
- **启动时 Ping**：`NewStore` 在返回前执行 `db.Ping()` 验证连接可达，确保服务启动时能及时发现数据库配置错误，而非等到第一次轮询才失败。

Store 还将 `errorBackoffSec` 和 `errorThreshold` 作为自身状态保存，供 `IsInErrorBackoff` 方法使用。

#### 4.3.2 队列查询

```go
func (s *Store) FetchQueuedRequests(ctx context.Context, limit int) ([]*WebhookRequest, error) {
    rows, err := s.db.QueryContext(ctx,
        `SELECT id, phid, webhookPHID, objectPHID, status, properties,
                lastRequestResult, lastRequestEpoch, dateCreated, dateModified
         FROM herald_webhookrequest
         WHERE status = ?
         ORDER BY id ASC
         LIMIT ?`,
        StatusQueued, limit)
    // ...
}
```

查询设计要点：

- **`ORDER BY id ASC`**：按 ID 升序取出，保证先进先出（FIFO）。更早创建的请求优先投递。
- **`LIMIT ?`**：每次最多取出 `maxConcurrent` 条记录，与并发上限匹配。避免一次取出过多记录导致内存压力，也避免同一批次中的请求在并发处理时竞争同一 webhook 的锁。
- **参数化查询**：使用 `?` 占位符防止 SQL 注入。
- **Context 传播**：通过 `QueryContext` 传入 context，确保服务关闭时（context 取消）正在执行的查询能及时中断。

#### 4.3.3 错误回退检查

```go
func (s *Store) IsInErrorBackoff(ctx context.Context, webhookPHID string) (bool, error) {
    cutoff := time.Now().Unix() - int64(s.errorBackoffSec)
    var count int64
    err := s.db.QueryRowContext(ctx,
        `SELECT COUNT(*) FROM herald_webhookrequest
         WHERE webhookPHID = ?
           AND lastRequestResult = ?
           AND lastRequestEpoch >= ?`,
        webhookPHID, ResultFail, cutoff).Scan(&count)
    if err != nil {
        return false, fmt.Errorf("check backoff: %w", err)
    }
    return count >= int64(s.errorThreshold), nil
}
```

错误回退机制的核心查询：统计某个 webhook 在 `errorBackoffSec` 秒窗口内的失败次数，与 `errorThreshold` 比较。

**滑动窗口设计**：`cutoff` 基于当前时间向前推 `errorBackoffSec` 秒，每次调用都重新计算窗口边界。这意味着：
- 随着时间推移，旧的失败记录自然"滑出"窗口，webhook 自动恢复投递——无需手动干预。
- 窗口大小和阈值可通过环境变量调整，适应不同的容错需求。
- 默认配置（300 秒窗口 / 10 次阈值）意味着：如果某个 webhook 在最近 5 分钟内连续失败 10 次，暂停投递；随着旧记录超出窗口，失败计数自然下降，低于阈值后恢复。

#### 4.3.4 结果更新

```go
func (s *Store) UpdateRequestResult(ctx context.Context, reqID int64, status, result,
    errorType, errorCode string, epoch int64, props *WebhookRequestProperties) error {
    propsJSON, err := json.Marshal(props)
    if err != nil {
        return fmt.Errorf("marshal properties: %w", err)
    }

    _, err = s.db.ExecContext(ctx,
        `UPDATE herald_webhookrequest
         SET status = ?, lastRequestResult = ?, lastRequestEpoch = ?,
             properties = ?, dateModified = ?
         WHERE id = ?`,
        status, result, epoch, string(propsJSON), time.Now().Unix(), reqID)
    if err != nil {
        return fmt.Errorf("update request: %w", err)
    }
    return nil
}
```

投递完成后将结果回写到数据库。更新的字段包括：

- `status`：最终状态（`sent`/`failed`/`queued`）。
- `lastRequestResult`：本次投递结果（`okay`/`fail`/`none`）。
- `lastRequestEpoch`：本次投递的时间戳（Unix 秒）。
- `properties`：更新后的属性 JSON（包含新写入的 `errorType`/`errorCode`）。
- `dateModified`：记录的修改时间戳。

`properties` 字段的回写将 gorge-webhook 的投递结果（错误类型、错误码等）持久化回 Phorge 数据库，使得 Phorge 的 Herald 管理界面能展示投递状态和错误详情。

#### 4.3.5 统计查询

```go
func (s *Store) Stats(ctx context.Context) (*DeliveryStats, error) {
    stats := &DeliveryStats{}

    err := s.db.QueryRowContext(ctx,
        `SELECT COUNT(*) FROM herald_webhookrequest WHERE status = ?`,
        StatusQueued).Scan(&stats.QueuedCount)
    // ... 分别查询 sent, failed, active webhooks ...
    return stats, nil
}
```

统计查询发送四条独立的 `SELECT COUNT(*)` 查询，分别统计各状态的请求数和活跃 webhook 数。使用多条简单查询而非一条复杂聚合查询的原因：

- 每条查询都很简单，MySQL 可以直接利用 `status` 字段的索引。
- 避免复杂的 `GROUP BY` 或子查询，降低数据库的查询优化压力。
- 任一查询失败时可以明确定位问题，调试更方便。

### 4.4 调度器（Dispatcher）

Dispatcher 位于 `internal/delivery/dispatcher.go`，是整个服务的核心引擎，实现了完整的 webhook 投递流水线。

#### 4.4.1 数据结构

```go
type Dispatcher struct {
    store         *Store
    client        *http.Client
    pollInterval  time.Duration
    maxConcurrent int
    backoffSec    int

    mu       sync.Mutex
    hookLock map[string]*sync.Mutex
}
```

Dispatcher 持有以下资源：

- `store`：数据库访问层，用于查询和更新请求。
- `client`：HTTP 客户端，配置了超时时间，用于向目标服务发送 POST 请求。
- `pollInterval`：轮询间隔。
- `maxConcurrent`：最大并发投递数。
- `backoffSec`：错误回退窗口秒数（传递给 Store 的回退检查，同时 Dispatcher 自身也持有一份用于日志等场景）。
- `hookLock`：per-webhook 的互斥锁映射，确保同一 webhook 的请求按顺序投递。

#### 4.4.2 轮询循环

```go
func (d *Dispatcher) Run(ctx context.Context) {
    log.Printf("[webhook] dispatcher started, poll=%s, concurrency=%d",
        d.pollInterval, d.maxConcurrent)

    sem := make(chan struct{}, d.maxConcurrent)
    ticker := time.NewTicker(d.pollInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            log.Println("[webhook] dispatcher shutting down")
            return
        case <-ticker.C:
            requests, err := d.store.FetchQueuedRequests(ctx, d.maxConcurrent)
            if err != nil {
                log.Printf("[webhook] poll error: %v", err)
                continue
            }
            for _, req := range requests {
                sem <- struct{}{}
                go func(r *WebhookRequest) {
                    defer func() { <-sem }()
                    d.processRequest(ctx, r)
                }(req)
            }
        }
    }
}
```

轮询循环的设计细节：

**Ticker 驱动**：使用 `time.NewTicker` 而非 `time.Sleep` 驱动轮询。Ticker 保证了稳定的轮询频率——即使一次轮询处理耗时较长，下一次轮询仍在固定间隔后触发（如果前一次已完成）。`defer ticker.Stop()` 确保 goroutine 退出时释放 Ticker 资源。

**信号量并发控制**：`sem := make(chan struct{}, d.maxConcurrent)` 创建一个带缓冲的 channel 作为信号量。每个请求处理前向 channel 写入一个空结构体（`sem <- struct{}{}`），处理完成后读出（`<-sem`）。当 channel 满时，新的请求处理会阻塞在写入操作上，直到有正在处理的请求完成。

**Context 感知**：`select` 语句同时监听 `ctx.Done()` 和 `ticker.C`。当服务收到终止信号时（通过 context 取消传播），轮询循环立即退出。进行中的请求处理通过 context 传播自然终止。

**错误容忍**：轮询查询失败时仅记录日志并 `continue`，不会中断整个轮询循环。数据库连接可能因网络抖动暂时失败，下次轮询时会自动重试。

#### 4.4.3 Per-Webhook 串行锁

```go
func (d *Dispatcher) getLock(webhookPHID string) *sync.Mutex {
    d.mu.Lock()
    defer d.mu.Unlock()
    lk, ok := d.hookLock[webhookPHID]
    if !ok {
        lk = &sync.Mutex{}
        d.hookLock[webhookPHID] = lk
    }
    return lk
}
```

`hookLock` 为每个 webhook 维护一个独立的互斥锁。设计目的：

**保证有序投递**：同一 webhook 的多个请求按顺序投递，避免目标服务收到乱序事件。例如，一个任务先被创建再被关闭，对应的两个 webhook 请求必须按创建顺序投递，否则目标服务可能收到"关闭"事件后才收到"创建"事件。

**允许跨 webhook 并行**：不同 webhook 的请求使用不同的锁，可以完全并行处理。这在有多个 webhook 配置时显著提升吞吐量。

**惰性创建**：锁在首次访问时创建，不预分配。`d.mu` 保护 `hookLock` map 的并发写入安全。

#### 4.4.4 请求处理流程

```go
func (d *Dispatcher) processRequest(ctx context.Context, req *WebhookRequest) {
    var props WebhookRequestProperties
    if err := json.Unmarshal([]byte(req.Properties), &props); err != nil {
        d.failPermanent(ctx, req, &props, ErrorTypeHook, "invalid-properties")
        return
    }

    hook, err := d.store.GetWebhook(ctx, req.WebhookPHID)
    if err != nil {
        return
    }
    if hook == nil {
        d.failPermanent(ctx, req, &props, ErrorTypeHook, "not-found")
        return
    }

    if hook.Status == "disabled" {
        d.failPermanent(ctx, req, &props, ErrorTypeHook, "disabled")
        return
    }

    lk := d.getLock(hook.PHID)
    lk.Lock()
    defer lk.Unlock()

    inBackoff, err := d.store.IsInErrorBackoff(ctx, hook.PHID)
    if err != nil {
        return
    }
    if inBackoff {
        return
    }

    d.deliverRequest(ctx, req, hook, &props)
}
```

处理流程分为六个步骤，每步都有明确的失败处理：

```
processRequest()
  │
  ├─ 1. 解析 Properties JSON
  │   └─ 失败 → failPermanent("invalid-properties")
  │
  ├─ 2. 查询 Webhook 配置
  │   ├─ 数据库错误 → 跳过（等待下次轮询）
  │   └─ 未找到 → failPermanent("not-found")
  │
  ├─ 3. 检查 Webhook 状态
  │   └─ disabled → failPermanent("disabled")
  │
  ├─ 4. 获取 Per-Webhook 锁（串行化）
  │
  ├─ 5. 检查错误回退
  │   ├─ 数据库错误 → 跳过
  │   └─ 在回退期内 → 跳过（保持 queued）
  │
  └─ 6. deliverRequest() → 实际投递
```

**永久失败 vs 临时跳过**：步骤 1/2/3 中的配置性错误（属性无效、webhook 不存在或被禁用）导致永久失败（`failPermanent`），因为这些问题不会因重试而改善。步骤 2/5 中的数据库查询错误和回退检查导致临时跳过——请求保持 `queued` 状态，下次轮询时重新处理。

**锁的获取时机**：锁在步骤 4 获取，位于配置检查之后。这意味着配置性错误（webhook 不存在或被禁用）不会触发锁等待——这类请求可以快速失败，不阻塞同一 webhook 的其他有效请求。

#### 4.4.5 投递执行

```go
func (d *Dispatcher) deliverRequest(ctx context.Context, req *WebhookRequest,
    hook *Webhook, props *WebhookRequestProperties) {

    objectType := phidGetType(req.ObjectPHID)

    triggers := make([]PayloadTrigger, 0, len(props.TriggerPHIDs))
    for _, phid := range props.TriggerPHIDs {
        triggers = append(triggers, PayloadTrigger{PHID: phid})
    }

    transactions := make([]PayloadTransaction, 0, len(props.TransactionPHIDs))
    for _, phid := range props.TransactionPHIDs {
        transactions = append(transactions, PayloadTransaction{PHID: phid})
    }

    payload := WebhookPayload{
        Object: PayloadObject{
            Type: objectType,
            PHID: req.ObjectPHID,
        },
        Triggers:     triggers,
        Action: PayloadAction{
            Test:   props.Test,
            Silent: props.Silent,
            Secure: props.Secure,
            Epoch:  req.DateCreated,
        },
        Transactions: transactions,
    }

    payloadBytes, err := json.MarshalIndent(payload, "", "  ")
    if err != nil {
        return
    }
    payloadStr := string(payloadBytes) + "\n"

    signature := computeHMACSHA256(payloadStr, hook.HmacKey)

    httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.WebhookURI,
        strings.NewReader(payloadStr))
    if err != nil {
        d.handleFailure(ctx, req, props, ErrorTypeHook, "request-build-error", time.Now().Unix())
        return
    }
    httpReq.Header.Set("Content-Type", "application/json")
    httpReq.Header.Set("X-Phabricator-Webhook-Signature", signature)

    start := time.Now()
    resp, err := d.client.Do(httpReq)
    duration := time.Since(start)

    now := time.Now().Unix()

    if err != nil {
        var errorType, errorCode string
        if ctx.Err() != nil || isTimeoutErr(err) {
            errorType = ErrorTypeTimeout
            errorCode = "timeout"
        } else {
            errorType = ErrorTypeHTTP
            errorCode = err.Error()
        }
        d.handleFailure(ctx, req, props, errorType, errorCode, now)
        return
    }
    defer func() { _ = resp.Body.Close() }()
    _, _ = io.ReadAll(resp.Body)

    statusCode := fmt.Sprintf("%d", resp.StatusCode)

    if resp.StatusCode >= 200 && resp.StatusCode < 300 {
        props.ErrorType = ErrorTypeHTTP
        props.ErrorCode = statusCode
        _ = d.store.UpdateRequestResult(ctx, req.ID, StatusSent, ResultOkay,
            ErrorTypeHTTP, statusCode, now, props)
        return
    }

    d.handleFailure(ctx, req, props, ErrorTypeHTTP, statusCode, now)
}
```

投递执行是整个服务最核心的函数，分为以下阶段：

**Payload 构建**：将数据库中的原始数据组装成目标服务期望的 JSON 结构。`phidGetType` 从 PHID 字符串中提取对象类型（如 `PHID-TASK-xxx` → `TASK`），让目标服务无需额外查询即可知道触发对象的类型。`json.MarshalIndent` 生成带缩进的 JSON，增强可读性（便于目标服务的日志和调试），末尾追加换行符与 Phorge 原始实现保持一致。

**HMAC-SHA256 签名**：使用 webhook 配置中的 `hmacKey` 对完整的 payload 字符串进行 HMAC-SHA256 签名，签名值通过 `X-Phabricator-Webhook-Signature` 请求头发送。目标服务可以用相同密钥重新计算签名并与请求头比较，验证请求确实来自 gorge-webhook 且 payload 未被篡改。

**HTTP 请求发送**：使用 `http.NewRequestWithContext` 将 context 传入请求，确保服务关闭时正在进行的 HTTP 请求能被取消。`d.client` 预配置了超时时间（默认 15 秒），防止目标服务无响应时无限等待。

**结果处理**：

| 情况 | 错误类型 | 处理方式 |
|------|----------|----------|
| HTTP 2xx 响应 | `http` | 标记 `sent`/`okay` |
| 超时或 context 取消 | `timeout` | 调用 `handleFailure` |
| 其他网络错误 | `http` | 调用 `handleFailure` |
| 非 2xx HTTP 响应 | `http` | 调用 `handleFailure` |

**响应体消费**：`io.ReadAll(resp.Body)` 在成功时也完整读取响应体。虽然 gorge-webhook 不关心响应内容，但不读取响应体会导致底层 TCP 连接无法复用（HTTP keep-alive 要求请求体被完整消费），影响后续请求的性能。

#### 4.4.6 失败处理

```go
func (d *Dispatcher) failPermanent(ctx context.Context, req *WebhookRequest,
    props *WebhookRequestProperties, errorType, errorCode string) {
    props.ErrorType = errorType
    props.ErrorCode = errorCode
    _ = d.store.UpdateRequestResult(ctx, req.ID, StatusFailed, ResultNone,
        errorType, errorCode, 0, props)
}

func (d *Dispatcher) handleFailure(ctx context.Context, req *WebhookRequest,
    props *WebhookRequestProperties, errorType, errorCode string, epoch int64) {
    props.ErrorType = errorType
    props.ErrorCode = errorCode

    if props.Retry == RetryForever {
        _ = d.store.UpdateRequestResult(ctx, req.ID, StatusQueued, ResultFail,
            errorType, errorCode, epoch, props)
    } else {
        _ = d.store.UpdateRequestResult(ctx, req.ID, StatusFailed, ResultFail,
            errorType, errorCode, epoch, props)
    }
}
```

两个失败处理函数对应不同的失败场景：

**`failPermanent`**：配置性错误导致的永久失败。请求直接标记为 `StatusFailed`、`ResultNone`（未尝试投递），`epoch` 传 0 表示没有实际的投递时间戳。这类失败不受重试策略影响——即使 `retry=forever`，webhook 不存在或属性无效的请求也无法通过重试修复。

**`handleFailure`**：投递级别的失败（网络错误、非 2xx 响应、超时）。此时重试策略生效：
- `retry=forever`：状态保持 `queued`，结果标记为 `fail`。下次轮询时重新处理。
- `retry=never`（或其他值）：状态设为 `failed`，不再重试。

两个函数都将错误信息回写到 `props`，确保 Phorge 界面能展示详细的错误原因。

#### 4.4.7 辅助函数

```go
func computeHMACSHA256(message, key string) string {
    mac := hmac.New(sha256.New, []byte(key))
    mac.Write([]byte(message))
    return hex.EncodeToString(mac.Sum(nil))
}

func phidGetType(phid string) string {
    parts := strings.SplitN(phid, "-", 3)
    if len(parts) >= 2 {
        return parts[1]
    }
    return ""
}

func isTimeoutErr(err error) bool {
    type timeouter interface {
        Timeout() bool
    }
    if te, ok := err.(timeouter); ok {
        return te.Timeout()
    }
    return false
}
```

**`computeHMACSHA256`**：标准的 HMAC-SHA256 计算。使用 `hex.EncodeToString` 输出十六进制字符串，与 Phorge 的签名格式一致。

**`phidGetType`**：Phorge 的 PHID 格式为 `PHID-{TYPE}-{hash}`，`SplitN(phid, "-", 3)` 最多切分为 3 段，取第二段作为类型标识。如 `PHID-TASK-abc123` → `TASK`，`PHID-DREV-xyz789` → `DREV`（Differential Revision）。

**`isTimeoutErr`**：通过接口断言（duck typing）检查错误是否为超时类型。Go 的 `net` 包中的超时错误实现了 `Timeout() bool` 方法。使用接口断言而非类型断言（`err.(*net.OpError)`），使得函数能兼容所有实现了 `Timeout()` 方法的错误类型，包括 HTTP 客户端包装的超时错误。

### 4.5 HTTP API

HTTP API 位于 `internal/httpapi/handlers.go`，基于 Echo 框架实现。

#### 4.5.1 路由注册

```go
type Deps struct {
    Store *delivery.Store
    Token string
}

func RegisterRoutes(e *echo.Echo, deps *Deps) {
    e.GET("/", healthPing())
    e.GET("/healthz", healthPing())

    g := e.Group("/api/webhook")
    g.Use(tokenAuth(deps))

    g.GET("/stats", stats(deps))
    g.GET("/hooks", listHooks(deps))
}
```

路由分为两层：

**公开路由**（`/`、`/healthz`）：无需认证的健康检查端点。两个路径功能相同，`/healthz` 是 Kubernetes/Docker 健康检查的惯用路径，`/` 提供便捷访问。

**认证路由**（`/api/webhook/*`）：通过 `tokenAuth` 中间件保护，需要有效的 Service Token。使用 Echo 的路由组（`Group`）机制，中间件自动应用于组内所有路由。

#### 4.5.2 认证中间件

```go
func tokenAuth(deps *Deps) echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            if deps.Token == "" {
                return next(c)
            }
            token := c.Request().Header.Get("X-Service-Token")
            if token == "" {
                token = c.QueryParam("token")
            }
            if token == "" || token != deps.Token {
                return c.JSON(http.StatusUnauthorized, &apiResponse{
                    Error: &apiError{
                        Code:    "ERR_UNAUTHORIZED",
                        Message: "missing or invalid service token",
                    },
                })
            }
            return next(c)
        }
    }
}
```

认证设计：

- **可选认证**：`SERVICE_TOKEN` 为空时跳过认证，方便开发环境和信任网络内的部署。
- **双来源 Token**：支持 `X-Service-Token` 请求头和 `token` 查询参数两种方式。请求头适合程序化调用，查询参数适合浏览器直接访问（如监控仪表盘的嵌入链接）。
- **请求头优先**：先检查请求头，为空时回退到查询参数。
- **统一错误格式**：使用 `apiResponse`/`apiError` 结构体返回 JSON 格式的错误信息，与正常响应格式一致。

#### 4.5.3 统计接口

```go
func stats(deps *Deps) echo.HandlerFunc {
    return func(c echo.Context) error {
        s, err := deps.Store.Stats(c.Request().Context())
        if err != nil {
            return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
        }
        return respondOK(c, s)
    }
}

func listHooks(deps *Deps) echo.HandlerFunc {
    return func(c echo.Context) error {
        ctx := c.Request().Context()
        row := deps.Store.DB().QueryRowContext(ctx,
            `SELECT COUNT(*) FROM herald_webhook`)
        var count int64
        if err := row.Scan(&count); err != nil {
            return respondErr(c, http.StatusInternalServerError, "ERR_INTERNAL", err.Error())
        }
        return respondOK(c, map[string]int64{"total": count})
    }
}
```

**`/api/webhook/stats`**：返回四项统计数据——等待投递数（queued）、已成功数（sent）、已失败数（failed）、活跃 webhook 数。这些数据可用于监控告警（如 queued 持续增长可能表示投递积压）。

**`/api/webhook/hooks`**：返回 webhook 总数。`listHooks` 直接通过 `deps.Store.DB()` 获取底层 `*sql.DB` 执行查询，而非在 Store 层封装方法。这种设计权衡：简单的一次性查询不值得在 Store 中增加一个方法，直接使用底层连接更直接。

### 4.6 入口程序

```go
func main() {
    cfg := config.LoadFromEnv()

    store, err := delivery.NewStore(cfg.HeraldDSN(), cfg.ErrorBackoffSec, cfg.ErrorThreshold)
    if err != nil {
        fmt.Fprintf(os.Stderr, "failed to connect to database: %v\n", err)
        os.Exit(1)
    }
    defer store.Close()

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    dispatcher := delivery.NewDispatcher(
        store, cfg.DeliveryTimeout, cfg.PollIntervalMs,
        cfg.MaxConcurrent, cfg.ErrorBackoffSec,
    )
    go dispatcher.Run(ctx)

    e := echo.New()
    e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
        LogStatus: true, LogURI: true, LogMethod: true,
        LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
            c.Logger().Infof("%s %s %d", v.Method, v.URI, v.Status)
            return nil
        },
    }))
    e.Use(middleware.Recover())

    httpapi.RegisterRoutes(e, &httpapi.Deps{
        Store: store,
        Token: cfg.ServiceToken,
    })

    go func() {
        sigCh := make(chan os.Signal, 1)
        signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
        <-sigCh
        cancel()
        _ = e.Shutdown(context.Background())
    }()

    e.Logger.Fatal(e.Start(cfg.ListenAddr))
}
```

启动流程的设计要点：

**依赖注入**：`Store` 作为共享依赖，同时注入 Dispatcher（投递引擎）和 HTTP API（统计查询）。两者通过同一个数据库连接池访问数据，避免连接资源的重复创建。

**并行运行**：Dispatcher 和 HTTP 服务器在独立的 goroutine 中并行运行。`go dispatcher.Run(ctx)` 启动后台轮询，`e.Start(cfg.ListenAddr)` 在主 goroutine 中阻塞监听 HTTP 请求。

**优雅关闭**：信号处理在独立 goroutine 中运行。收到 `SIGINT` 或 `SIGTERM` 后：
1. `cancel()` 取消 context，触发 Dispatcher 停止轮询。
2. `e.Shutdown()` 优雅关闭 HTTP 服务器——完成正在处理的请求后停止接受新请求。
3. 主 goroutine 中的 `e.Start()` 返回，触发 `defer store.Close()` 关闭数据库连接。

**中间件**：
- `RequestLogger`：记录每个 HTTP 请求的方法、URI 和状态码，便于运维排查。
- `Recover`：捕获 handler 中的 panic，返回 500 而非进程崩溃。

## 5. 并发模型

### 5.1 并发控制层级

gorge-webhook 的并发控制分为三层：

```
全局信号量 (chan struct{}, maxConcurrent)
  └── 限制同时处理的请求总数
      │
      ▼
Per-Webhook 互斥锁 (hookLock map[string]*sync.Mutex)
  └── 同一 webhook 的请求串行处理
      │
      ▼
单次 HTTP 请求 (http.Client 超时)
  └── 防止单次投递无限等待
```

**第一层**：全局信号量限制整个系统的并发投递数上限（默认 8）。即使有多个 webhook 各自有待处理的请求，系统也不会同时发起超过 8 个 HTTP 请求。这保护了 gorge-webhook 自身的资源（内存、文件描述符、CPU）。

**第二层**：per-webhook 互斥锁保证同一 webhook 的请求按 FIFO 顺序处理。不同 webhook 之间的锁是独立的，互不干扰。这在保证有序性的同时最大化了并行度。

**第三层**：HTTP 客户端超时（默认 15 秒）防止单次投递因目标服务无响应而无限阻塞，最终导致信号量永远无法释放。

### 5.2 并发场景分析

**正常场景**：轮询取出 N 个请求（N ≤ maxConcurrent），每个请求在独立 goroutine 中处理。属于不同 webhook 的请求完全并行，属于同一 webhook 的请求排队等待锁。

**高负载场景**：队列中有大量待处理请求。信号量限制了同时处理的数量，超出部分在 `sem <- struct{}{}` 处阻塞，等待前序请求完成。由于每次轮询最多取 `maxConcurrent` 条记录，阻塞的情况主要发生在同一批次中有多个属于同一 webhook 的请求时。

**目标服务故障场景**：某个 webhook 的目标服务不可达或响应缓慢。该 webhook 的请求会因 HTTP 超时而快速失败（15 秒），释放信号量。随着失败次数在回退窗口内累积到阈值，`IsInErrorBackoff` 返回 true，该 webhook 的后续请求被跳过，不再占用并发资源。

**锁竞争场景**：一个 webhook 有大量待处理请求，多个轮询批次取出同一 webhook 的多个请求。`hookLock` 确保它们串行处理，但锁等待不消耗信号量——请求在 `lk.Lock()` 处等待时已经占用了一个信号量槽位。如果大量请求等待同一 webhook 的锁，可能导致其他 webhook 的请求无法获得信号量。这是 per-webhook 串行化的固有代价，通过合理设置 `maxConcurrent` 和 `pollIntervalMs` 来缓解。

## 6. 错误回退（Error Backoff）

### 6.1 机制设计

错误回退是一种保护机制，防止对持续故障的目标服务进行无效的反复投递。

```
                    errorBackoffSec (默认 300s) 滑动窗口
              ◄─────────────────────────────────────────►
              │                                         │
时间轴:  ─────┼──X──X──X──X──X──X──X──X──X──X──────────┼──────
              │  10 次失败                              │
              │                                         │
              │  count >= errorThreshold (10)            │
              │  → 进入回退，跳过该 webhook 的所有请求   │
              │                                         │
              │          随时间推移，旧失败滑出窗口       │
              │          → count 下降 → 自动恢复         │
```

### 6.2 优势

- **自动恢复**：基于滑动窗口而非固定标记，不需要手动"重置"。目标服务恢复后，旧的失败记录自然超出窗口，失败计数下降到阈值以下，投递自动恢复。
- **无额外状态**：利用已有的 `lastRequestResult` 和 `lastRequestEpoch` 字段计算，不需要在 gorge-webhook 内存中维护额外的状态或使用独立的计数器。
- **可调参数**：`ERROR_BACKOFF_SEC` 和 `ERROR_THRESHOLD` 可通过环境变量调整。高容忍场景可以增大阈值或窗口，严格场景可以缩小。

### 6.3 与重试策略的交互

回退检查发生在重试之前：

1. 请求被取出时处于 `queued` 状态（可能是新请求，也可能是 `retry=forever` 的失败重试请求）。
2. 检查该 webhook 是否在错误回退期内。
3. 如果在回退期内，直接跳过——请求保持 `queued`，等待下次轮询时再次检查。
4. 如果不在回退期内，执行投递。投递失败且 `retry=forever` 时，请求回到 `queued` 状态。

这意味着 `retry=forever` 的请求在回退期内不会丢失——它们保持 `queued` 状态，回退期结束后自动重新投递。

## 7. 安全机制

### 7.1 HMAC-SHA256 签名

每次投递都使用 webhook 配置中的 `hmacKey` 对 payload 进行 HMAC-SHA256 签名：

```
payload (JSON 字符串)  +  hmacKey (webhook 配置)
         │                       │
         └───────┬───────────────┘
                 │
          HMAC-SHA256
                 │
                 ▼
    X-Phabricator-Webhook-Signature: <hex 签名>
```

目标服务的验证流程：
1. 从请求头获取 `X-Phabricator-Webhook-Signature`。
2. 从请求体读取原始 payload 字符串。
3. 使用预共享的 HMAC 密钥重新计算签名。
4. 用常量时间比较函数对比两个签名。匹配则请求可信。

签名头使用 `X-Phabricator-Webhook-Signature` 而非自定义名称，保持与 Phorge 原始实现的兼容性——现有的 webhook 接收端无需修改即可验证 gorge-webhook 发出的请求。

### 7.2 API Token 认证

HTTP API 的认证使用简单的静态 Token 比较。适用场景：内部网络中的服务间通信，Token 通过环境变量注入，不暴露给外部。

`SERVICE_TOKEN` 为空时跳过认证的设计考量：在信任网络中（如 Kubernetes 集群内部），服务间通信通过网络策略保护，Token 认证是非必要的。允许跳过认证简化了开发和测试环境的配置。

## 8. 部署方案

### 8.1 Docker 镜像

采用多阶段构建：

```dockerfile
FROM golang:1.26-alpine3.22 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o gorge-webhook ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /src/gorge-webhook /usr/local/bin/gorge-webhook
EXPOSE 8160
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8160/healthz || exit 1
ENTRYPOINT ["gorge-webhook"]
```

构建要点：

- **依赖缓存**：先复制 `go.mod`/`go.sum` 并 `go mod download`，再复制源码。依赖不变时利用 Docker 层缓存跳过下载。
- **静态编译**：`CGO_ENABLED=0` 禁用 CGO，生成纯静态二进制。`-ldflags="-s -w"` 去除调试信息和符号表，缩小二进制体积。
- **CA 证书**：运行阶段安装 `ca-certificates`，确保 HTTPS webhook 目标的 TLS 证书验证正常。
- **健康检查**：Docker 内置 HEALTHCHECK，每 10 秒通过 `wget` 检查 `/healthz` 端点。`start-period=5s` 给服务启动预留时间，`retries=3` 允许偶尔的健康检查失败。

### 8.2 部署拓扑

```
┌─────────────────────────────────────────┐
│           Docker / Kubernetes           │
│                                         │
│  ┌───────────┐       ┌───────────────┐  │
│  │  Phorge   │       │ gorge-webhook │  │
│  │  (PHP)    │       │               │  │
│  └─────┬─────┘       └───────┬───────┘  │
│        │                     │          │
│        │    ┌───────────┐    │          │
│        └───▶│   MySQL   │◀───┘          │
│             └───────────┘               │
└─────────────────────────────────────────┘
```

gorge-webhook 与 Phorge 共享同一个 MySQL 实例。两者通过 Herald 数据库表进行数据交互，不需要直接的网络通信。

### 8.3 部署建议

**数据库权限**：gorge-webhook 的 MySQL 用户只需要 `{namespace}_herald` 数据库的 `SELECT` 和 `UPDATE` 权限。不需要 `INSERT`、`DELETE` 或 DDL 权限——请求的创建和 webhook 的管理由 Phorge 负责。

**网络隔离**：gorge-webhook 的 HTTP API（`:8160`）仅提供只读的统计信息，不接受外部请求提交。可以通过防火墙规则限制为仅内部网络可达。

**资源估算**：gorge-webhook 的资源消耗主要取决于并发投递数。每个并发投递占用一个 goroutine（~4KB 栈内存）和一个 HTTP 连接。默认 8 个并发的配置下，额外内存消耗不超过 1MB。Go 运行时本身约需 10-20MB，数据库连接池（20 个连接）约需 5MB，总体内存占用在 30-40MB 级别。

**参数调优**：

| 参数 | 低负载场景 | 高负载场景 | 说明 |
|------|-----------|-----------|------|
| `POLL_INTERVAL_MS` | 2000-5000 | 500-1000 | 降低轮询频率减少数据库压力 |
| `MAX_CONCURRENT` | 4 | 16-32 | 增加并发提升吞吐量 |
| `DELIVERY_TIMEOUT` | 15-30 | 5-10 | 缩短超时加速失败检测 |
| `ERROR_BACKOFF_SEC` | 600 | 120-300 | 高负载时缩短回退窗口加速恢复 |
| `ERROR_THRESHOLD` | 5-10 | 15-20 | 高负载时提高容忍度 |

## 9. 依赖分析

| 依赖 | 版本 | 用途 |
|------|------|------|
| `github.com/labstack/echo/v4` | v4.15.1 | HTTP 框架，路由、中间件、请求日志 |
| `github.com/go-sql-driver/mysql` | v1.9.3 | MySQL 数据库驱动 |

直接依赖仅两个。

选择 Echo 框架而非标准库 `net/http` 的原因：Echo 提供了路由分组、中间件链、结构化日志等开箱即用的功能，减少了样板代码。对于一个需要认证中间件和多路由的 API 服务，使用框架的收益大于引入依赖的成本。

选择 `go-sql-driver/mysql` 的原因：这是 Go 生态中最成熟的 MySQL 驱动，实现了标准库 `database/sql` 接口。广泛使用、持续维护、性能优秀。

其他功能（HMAC 签名、JSON 编解码、HTTP 客户端、并发控制）全部使用 Go 标准库，无额外依赖。

## 10. 与 Phorge 原始实现的对应关系

| gorge-webhook | Phorge PHP | 差异说明 |
|---------------|------------|----------|
| `Dispatcher.Run()` 后台轮询 | PHP 请求中同步投递 | 异步解耦，不阻塞用户请求 |
| `Store.FetchQueuedRequests()` | Herald 引擎写入 | 分工明确：Phorge 写入，gorge-webhook 消费 |
| `deliverRequest()` | `PhabricatorWebhookEngine` | 投递逻辑一致：构建 payload、HMAC 签名、POST |
| `computeHMACSHA256()` | PHP `hash_hmac('sha256', ...)` | 签名算法和格式完全一致 |
| `X-Phabricator-Webhook-Signature` | 同名请求头 | 保持兼容，目标服务无需修改 |
| `WebhookPayload` 结构 | PHP payload 格式 | JSON 结构一致 |
| `handleFailure()` 重试策略 | `HeraldWebhookRequest` 重试逻辑 | 遵循相同的 retry/never/forever 语义 |
| `IsInErrorBackoff()` | PHP backoff 检查 | 滑动窗口机制一致 |
| `Store.UpdateRequestResult()` | PHP 结果回写 | 更新相同的表和字段 |

## 11. 总结

gorge-webhook 是一个职责单一的 webhook 异步投递微服务，核心价值在于：

1. **异步解耦**：将 webhook 投递从 Phorge 的 PHP 请求处理流程中剥离，消除了同步阻塞对用户体验的影响。Phorge 只负责将请求写入数据库，投递工作完全由 gorge-webhook 异步完成。
2. **无侵入集成**：不修改 Phorge 代码，通过 MySQL 共享数据库表实现松耦合集成。两者的契约是 `herald_webhookrequest` 和 `herald_webhook` 的表结构和字段语义。
3. **可靠投递**：三层并发控制（信号量 + per-webhook 锁 + HTTP 超时）保证了系统的稳定性；重试策略（`retry=forever`）和错误回退（滑动窗口）保证了投递的可靠性和对故障目标的自我保护。
4. **协议兼容**：HMAC-SHA256 签名、`X-Phabricator-Webhook-Signature` 请求头、JSON payload 结构均与 Phorge 原始实现保持一致，现有的 webhook 接收服务无需修改。
5. **极简实现**：直接依赖仅 Echo 和 MySQL 驱动两个库，核心投递逻辑约 250 行，数据访问约 160 行，HTTP API 约 100 行。代码结构清晰，易于理解和维护。
