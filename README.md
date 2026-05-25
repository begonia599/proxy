# claude-proxy

一个面向单管理员部署的 Anthropic Messages API 透明代理,自带:

- **字节级原样转发**:请求/响应 body 不重序列化,保证 prompt cache 正常命中
- **多代理 key + 单上游 key**:用 `sk-proxy-` 前缀的虚拟 key 分发给不同消费者,统一回真实 Anthropic key
- **每 key 独立策略**:每日预算上限、可用模型白名单、备注、随时吊销
- **模型 curation**:从上游拉模型列表,管理员可单独 enable/disable,未知/禁用模型直接 404 不浪费上游一跳
- **成本核算**:按模型最新单价 + 5m/1h ephemeral cache + web_search 次数实时计算 USD
- **完整审计日志**:每条请求都记录到 SQLite,非 2xx 状态额外保存请求/响应 body 快照(单边截断到 256KB)
- **管理面板**:`/admin/` 提供概览(9 卡 + 5 线折线图)、密钥管理、模型 curation、日志侧抽屉

## 适用场景

- 想给团队/朋友/工具分发 Anthropic 访问权,但**不想直接暴露官方 key**
- 想给每个使用者**独立预算上限**,防失控烧钱
- 想集中**审计每条请求**(谁、什么模型、多少 token、多少钱)
- 想**屏蔽部分模型**(比如禁用 Opus,只放 Haiku/Sonnet)

## 不适用场景

- 多管理员协作(没有用户系统,只有 admin token)
- 公开自助注册(前端不开放给非管理员)
- OpenAI 格式翻译(用 LiteLLM)

## 快速开始

### 1. 编译

```bash
git clone https://github.com/begonia599/proxy.git
cd proxy
go build -o claude-proxy .
```

需要 Go 1.21+。依赖 `modernc.org/sqlite`(纯 Go,无需 CGO)。

### 2. 配置 .env

复制示例:

```bash
cp .env.example .env
```

编辑 `.env`,填入真实 Anthropic key 和自定义 admin token:

```
key=sk-ant-api03-XXXXXXXXX...
admin_token=<你的强随机 token,32+ 字符>
```

> 程序会先在当前目录找 `.env`,找不到再退到 `../.env`。

### 3. 启动

```bash
./claude-proxy
```

输出:

```
proxy keys loaded: 0 active
model registry: 9 enabled models (upstream returned 9)
claude-proxy listening on :8787, upstream https://api.anthropic.com
admin token: <your token>
```

### 4. 打开管理面板

浏览器访问 `http://127.0.0.1:8787/admin/`,在前端粘贴你的 admin token 登录,然后:

- **密钥**页 → "新建密钥",填 owner / 每日预算 / 允许的模型,生成 `sk-proxy-xxxxxxxx` 给消费者
- **模型**页 → 按需禁用不想暴露的模型
- **日志**页 → 看每条请求详情,非 2xx 可在抽屉里查看完整 body
- **概览**页 → 折线图看 token / 缓存 / 成本趋势

### 5. 客户端调用

把 Anthropic SDK 的 `base_url` 指向你的代理,`api_key` 换成 `sk-proxy-xxx`:

```python
from anthropic import Anthropic
client = Anthropic(
    base_url="http://your-proxy-host:8787",
    api_key="sk-proxy-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
)
client.messages.create(
    model="claude-haiku-4-5-20251001",
    max_tokens=128,
    messages=[{"role": "user", "content": "hi"}],
)
```

curl 同理:

```bash
curl http://127.0.0.1:8787/v1/messages \
  -H "x-api-key: sk-proxy-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d '{"model":"claude-haiku-4-5-20251001","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
```

## Admin API

所有 `/admin/*` 都要带 `Authorization: Bearer <admin_token>`。

| 方法 + 路径 | 说明 |
|---|---|
| `GET  /admin/stats?since=&until=&proxy_key=&model=` | 总览统计(总数 + 按 key/model/天分组) |
| `GET  /admin/timeseries?granularity=hour\|day&...` | 时序桶,给折线图用 |
| `GET  /admin/keys` | 列出所有 proxy key |
| `POST /admin/keys` | 新建 key(body: `{owner, daily_budget, allowed_models, notes}`) |
| `PATCH /admin/keys/<key>` | 修改 owner/预算/白名单/备注 |
| `DELETE /admin/keys/<key>` | 吊销 |
| `GET  /admin/models` | 列出 curated 模型 |
| `PATCH /admin/models/<id>` | 开关启用状态(body: `{"enabled": true/false}`) |
| `GET  /admin/logs?proxy_key=&model=&status=&since=&until=&before_id=&limit=` | 请求日志列表,游标分页 |
| `GET  /admin/logs/<id>` | 单条日志详情(含 4xx/5xx 时的请求/响应 body) |

## 部署建议

- **改 admin_token**:默认 `admin-secret-change-me` 不安全
- **别公网裸暴露 8787**:这是单机 admin 工具,前面挡一层 nginx + IP 白名单 / VPN / Tailscale
- **HTTPS**:如果要外部访问,套个反向代理终止 TLS
- **数据库备份**:`claude-proxy.db` 包含所有 proxy key 和请求日志,定期备份
- **日志保留**:目前没有自动清理,长期跑可以加 cron `DELETE FROM requests WHERE ts < ?` 配合 `VACUUM`

## 架构

```
client (sk-proxy-xxx)
    ↓
[claude-proxy :8787]
    ├─ key 校验 / 预算检查 / model allowlist  (in-memory KeyCache)
    ├─ httputil.ReverseProxy → upstream
    ├─ tee body → SSE/JSON usage 解析 → SQLite (requests + error_bodies)
    └─ /admin/* → JSON API + embedded dashboard.html
    ↓ (x-api-key: real)
api.anthropic.com
```

- `main.go` 反向代理 + admin 路由
- `keys.go` proxy key 缓存
- `models.go` 模型注册表 + 每 30 分钟从上游刷新
- `pricing.go` 各模型 token 单价 + web_search 单价
- `storage.go` SQLite 持久化层(schema、查询、日志、时序)
- `dashboard.html` 单文件管理面板,go:embed

## License

MIT
