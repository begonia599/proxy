# claude-proxy

一个面向单管理员部署的 Anthropic Messages API 透明代理,自带:

- **字节级原样转发**:请求/响应 body 不重序列化,保证 prompt cache 正常命中
- **多代理 key + 单上游 key**:用 `sk-proxy-` 前缀的虚拟 key 分发给不同消费者,统一回真实 Anthropic key
- **每 key 独立策略**:每日预算上限、绑定一个分组作为可用模型白名单、备注、随时吊销
- **多服务商 + 分组映射**:接入多家上游(服务商),按需拉取其上游模型并手动选入模型库存(大组),在分组里把库存映射成对外逻辑名;分组可设「透传服务商」整家放行。proxy key 绑定一个分组
- **成本核算**:按服务商-模型计价(可逐模型覆盖,缺省回落静态 Anthropic 价表) + 5m/1h ephemeral cache + web_search 次数实时计算 USD
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

- **密钥**页 → "新建密钥",填 owner / 每日预算 / 绑定分组,生成 `sk-proxy-xxxxxxxx` 给消费者
- **服务商**页 → 接入上游,点「管理模型」拉取上游目录、勾选加入大组并设价;**分组**页 → 把大组里的模型映射成逻辑名或设透传服务商
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
| `POST /admin/keys` | 新建 key(body: `{owner, daily_budget, group_id, notes}`) |
| `PATCH /admin/keys/<key>` | 修改 owner/预算/绑定分组/备注 |
| `DELETE /admin/keys/<key>` | 吊销 |
| `GET  /admin/providers/<id>/catalog` | 拉取该服务商上游实时模型列表(不写库,供策展) |
| `POST /admin/providers/<id>/models` | 批量把选中模型加入大组(body: `{upstream_ids:[...]}`) |
| `DELETE /admin/providers/<id>/models/<upstream>` | 把模型移出大组 |
| `POST /admin/providers/<id>/models/<upstream>/price` | 设/清某模型计价覆盖(body: `{input,output,cache_write_5m,cache_write_1h,cache_read}` 或 `{"clear":true}`) |
| `GET/POST/PATCH/DELETE /admin/providers[/<id>]` | 服务商 CRUD;`GET /<id>/models` 列已加入大组的模型 |
| `GET/POST/PATCH/DELETE /admin/groups[/<id>/mappings...]` | 分组 + 逻辑名映射 CRUD;分组 PATCH 可设 `passthrough_provider_id` |
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
- `routing.go` 下游模型名 → 上游服务商/真实名解析(分组路由 + 透传)
- `providers.go` / `providers_store.go` 服务商 + 分组映射注册表与持久化
- `pricing.go` 静态价表 + 按服务商计价覆盖 + web_search 单价
- `storage.go` SQLite 持久化层(schema、查询、日志、时序)
- `dashboard.html` 单文件管理面板,go:embed

## License

MIT
