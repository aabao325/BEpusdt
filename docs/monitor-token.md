# 节点成功率监控接口 · 取令牌与接入指南

面向宝塔面板 + Docker Compose 部署，配合 changedetection 做节点成功率告警。

**前提**：宝塔后台要能看到「终端」，服务器上有 Docker。

---

## 最终答案（只想拿地址就看这段）

阈值 90、每 5 分钟轮询一次，用这个：

```
https://你的域名/api/monitor/stats?token=你的新令牌&threshold=90&samples=100
```

**不要加 `window=total`**，也不要加 `stale=0`。为什么是 `samples=100` 而不是默认值，
下面「窗口要匹配轮询间隔」一节有完整推导。

changedetection 里触发条件盯这个字符串：

```
"status": "OK"
```

---

## 一图看懂流程

```
① 推代码到 GitHub          → Actions 自动构建镜像到 ghcr.io
                                （约 3~5 分钟，看 Actions 页面绿勾）
② 宝塔终端 docker compose pull && up -d   → 更新到新镜像
③ docker exec bepusdt bepusdt monitor     → 拿到 32 位令牌
④ 浏览器访问接口确认返回 JSON
⑤ 地址填进 changedetection，5 分钟一轮
```

---

## 自动构建镜像到 ghcr.io

已经加好了 `.github/workflows/docker-ghcr.yml`。**不需要配置任何 Secret**，
它用 Actions 自带的 `GITHUB_TOKEN` 登录 ghcr。

### 触发时机

- 推送到 `main` 分支自动触发（只改 `.md` 或 `docs/` 不触发，省额度）
- 打 `v*` 标签触发
- Actions 页面手动点「Run workflow」，可以选架构

### 镜像地址

```
ghcr.io/aabao325/bepusdt:latest      # 跟随 main 最新
ghcr.io/aabao325/bepusdt:<7位提交号>  # 固定某次提交，方便回滚
```

注意是全小写的 `bepusdt`——ghcr 不接受大写镜像名，workflow 里已自动转换。

### 首次使用要做一件事：把包设为公开

镜像刚发布时默认是私有的，宝塔拉取会报 `unauthorized` 或 `denied`。

去 GitHub → 你的头像 → **Packages** → 点 `bepusdt` → 右侧 **Package settings**
→ 拉到底部 **Change visibility** → 选 **Public**。

只需做一次。之后 `docker pull` 不用登录。

> 如果你希望镜像保持私有，那就得在服务器上登录 ghcr：
> `docker login ghcr.io -u 你的GitHub用户名`，密码用 GitHub Personal Access
> Token（需要 `read:packages` 权限）。个人项目一般设公开更省事。

### 改 docker-compose.yml 用 ghcr 镜像

把 `build: .` 去掉，镜像名换成 ghcr 的：

```yaml
services:
  bepusdt:
    image: ghcr.io/aabao325/bepusdt:latest
    container_name: bepusdt
    restart: unless-stopped
    ports:
      - "127.0.0.1:8888:8080"
    volumes:
      - ./data:/var/lib/bepusdt
```

`build: .` 留着也不报错，但 `docker compose pull` 会去拉远端镜像，本地构建就没意义了。
另外原来挂载的 `./conf.toml` 现在代码里已经不读取，可以一起删掉。

### 以后更新只要两条命令

```bash
docker compose pull
docker compose up -d
```

想省点空间，顺手清理旧镜像：

```bash
docker image prune -f
```

### 关于构建架构

默认只构建 `linux/amd64`，绝大多数服务器（包括宝塔常见的云主机）都是这个。
构建约 3~5 分钟。

如果你的服务器是 ARM（比如某些 ARM 云实例），去 Actions 页面手动运行 workflow，
架构选 `linux/amd64,linux/arm64`。

> 我顺手改了 `dockerfile`（注意本仓库这个文件名是全小写的）：让前端编译和 Go 编译
> 都固定在构建机架构上跑，靠 `CGO_ENABLED=0` 交叉编译出目标架构的二进制。
> 不这么做的话，多架构构建时 arm64 那一路要在 QEMU 模拟下重跑一遍 pnpm 前端编译，
> 可能要二十多分钟甚至超时。

---

## 第 1 步：进入宝塔的终端

宝塔面板左侧菜单有个「终端」（有些版本叫「SSH 终端」）。点进去，会看到一个
黑色命令行窗口，这就是你宿主机的 shell。

如果宝塔的终端用不了（部分版本会连不上），用任意 SSH 工具连服务器也一样，
后面的命令没区别。

进去以后先切到项目目录。你的 `docker-compose.yml` 在哪，就 `cd` 到哪：

```bash
cd /www/wwwroot/bepusdt    # 换成你自己的实际路径
ls
```

`ls` 应该能看到 `docker-compose.yml`、`Dockerfile`、`data` 这些。看不到就说明
路径不对，先找对目录再往下走。

不确定路径的话，可以让 Docker 自己告诉你：

```bash
docker inspect bepusdt --format '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}'
```

---

## 第 2 步：拉取新镜像并重启

先确认 GitHub Actions 已经构建完（仓库 → Actions 页面，最新那条是绿勾），
然后在项目目录下执行：

```bash
docker compose pull
docker compose up -d
```

如果你的服务器上是老版本 Docker，`docker compose` 要写成 `docker-compose`
（中间是横线）。两个都试一下，能跑通哪个用哪个。

确认容器起来了：

```bash
docker compose ps
```

`STATUS` 那列显示 `Up` 就正常。

拉取报 `unauthorized` / `denied`，是包还是私有的，回上面「首次使用要做一件事」那节。

> **注意**：`docker compose up -d` 会重启容器。重启会清掉后台登录会话（内存态），
> 你需要重新登录后台。订单数据和令牌都在 `./data` 里，不受影响。

顺手确认一下版本对不对：

```bash
docker exec bepusdt bepusdt version
```

---

## 第 3 步：取监控令牌

```bash
docker exec bepusdt bepusdt monitor
```

`bepusdt` 出现两次不是笔误：第一个是容器名，第二个是容器里的程序名。

正常会输出：

```
┏━━  📡  节点状态监控
┃
┃    🎫  监控令牌:  6617A17187B64BFEEF50736B9BFCD726
┃
┃    接口地址（GET，替换为你的实际域名）:
┃    https://your-domain/api/monitor/stats?token=6617A17187B64BFEEF50736B9BFCD726
┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

把那串 32 位的令牌记下来。

不用加 `--sqlite` 参数：命令的默认数据库路径 `/var/lib/bepusdt/sqlite.db`
正好是你 compose 里 `./data` 挂进去的位置，能直接读到。

这个令牌**存在数据库里，不会过期**。容器重启、后台登录、改密码、退出登录都不影响它。
重复执行上面的命令，拿到的是同一个令牌。

### 令牌泄露了怎么办

```bash
docker exec bepusdt bepusdt monitor --reset
```

会生成新令牌，旧的立即失效。记得同步更新 changedetection 里的地址。

---

## 第 4 步：验证接口能通

把 `your-domain` 换成你实际的域名，令牌换成你自己的，浏览器里访问：

```
https://你的域名/api/monitor/stats?token=你的令牌
```

能返回一段 JSON 就通了。大概长这样：

```json
{
  "status": "OK",
  "alert": false,
  "alerts": [],
  "summary": { "networks": 1, "scanning": 1, "alerting": 0 },
  "networks": [
    {
      "network": "bsc",
      "block": "115052890",
      "succ": "100.00%",
      "recent_rate": 100,
      "stale_seconds": 3,
      "scanning": true,
      "alert": false
    }
  ]
}
```

> **注意**：compose 里端口映射是 `127.0.0.1:8888:8080`，意味着容器只监听本机的
> `8888` 端口，外部（包括你直接敲 `http://服务器IP:8888`）是访问不到的。你要从
> 浏览器/changedetection 访问，必须经过一层反向代理把 `https://你的域名` 转到
> `127.0.0.1:8888`。宝塔里通常在「网站 → 反向代理」里配。
>
> 配完后验证用的就是上面那个 `https://你的域名/...` 地址，不是 IP 加端口。

令牌不对或没带，会返回 `{"code":403,"msg":"invalid monitor token"}`，HTTP 状态码 403。

---

## 第 5 步：接 changedetection

1. 新建一个监控，URL 填：
   ```
   https://你的域名/api/monitor/stats?token=你的令牌&threshold=90&samples=100
   ```
2. **方法选 GET**，**检查间隔设 5 分钟**（`samples=100` 就是按这个间隔配的）。
3. 触发条件选「文本包含 / 关键字触发」，把下面的字符串设为变化基准：

   ```
   "status": "OK"
   ```

   changedetection 检测到响应里 `"status"` 的值从 `OK` 变成别的（`DEGRADED` 或
   `NO_DATA`）就会通知你。

4. 想用更精确的触发，可以开「JSON 过滤」，表达式取 `$.alert`，盯它从 `false`
   变 `true`。

告警触发时 `alerts` 数组里会带原因，比如：

```json
"alerts": ["bsc: 成功率 0.00% 低于阈值 50.00%"]
```

---

## 告警逻辑（读懂了才知道为什么会告警 / 为什么不告）

接口返回里最顶层两个字段：

| 字段 | 含义 |
|---|---|
| `status` | `OK` 正常 / `DEGRADED` 有网络告警 / `NO_DATA` 没有任何扫块样本 |
| `alert` | `true` = 至少有一条告警，`false` = 没有 |

每条网络的告警来源有两个维度，任一命中就告：

1. **成功率低**：`recent_rate`（最近 `samples` 次扫块的成功率，默认 20 次）低于
   `threshold`。这是主告警源。
2. **同步陈旧**：`stale_seconds`（距上次成功同步的秒数）超过阈值，默认 300 秒。
   这个维度**只在 `scanning: true` 时才参与**，下面会解释。

### 阈值是「平均值跌破线」，不是瞬时值

阈值不是「掉到这个数就立刻告警」。它比较的是窗口内的平均成功率，所以窗口多大
直接决定反应多快。具体数字见下面「可调参数」一节的对照表。

`samples=100` + `threshold=90` 的含义是：**最近 100 次扫块里有 11 次及以上失败就告警**。
偶发的一两次失败会被容忍，不会被网络抖动惊动。

### 边界：恰好等于阈值不会告警

判断是严格小于（`<`）。阈值 90 时，成功率正好 90.00% **不触发**。要包含等于，
写 `threshold=90.01`。

### 什么是 `scanning`，为什么空闲时不会误报陈旧

扫块是**需求驱动**的：没有待处理的订单、也没有启用「其它通知」的钱包时，扫块器
会主动停。这时数据不刷新是正常的，拿陈旧度告警会天天误报。

所以每条网络有 `scanning` 字段，告诉你当前**应该**在扫吗：

- `true`：有活要干，陈旧维度参与告警；
- `false`：空闲停扫，陈旧维度不参与，不会因为「好久没同步」而报警。

如果你的网关平时没启用 `other_notify` 的钱包，空闲时段接口可能返回
`networks: []` + `NO_DATA`。这个状态**刻意不置 `alert`**（对空闲网关是常态），
但 `status` 字符串和 `OK` 不同——你盯 `"status": "OK"` 依然会收到变化通知，
收到后看一眼 `scanning` 就知道是空闲还是故障。

---

## 可调参数（URL 后面拼）

| 参数 | 默认 | 取值范围 | 说明 |
|---|---|---|---|
| `threshold` | `50` | 0~100 | 成功率告警阈值（百分比），**严格低于**此值触发 |
| `samples` | `20` | 1~1000 | 近期窗口取多少个最新样本，决定灵敏度和覆盖时长 |
| `stale` | `300` | ≥ 0 | 陈旧秒数阈值，超过触发；`0` 关闭该维度 |
| `window` | `recent` | `recent` / `total` | `recent` 用 `samples` 个样本，`total` 用全部 1000 个 |

**5 分钟轮询的推荐配置**：

```
https://你的域名/api/monitor/stats?token=令牌&threshold=90&samples=100
```

参数之间用单个 `&` 分隔。**不要写成 `&amp;`**，否则参数不生效（见下面排查一节）。

### 窗口要匹配轮询间隔（这条最容易被忽略）

窗口不只影响灵敏度，还决定**这次请求能看到多久以前的事**。如果窗口覆盖的时间比
轮询间隔短，两次轮询之间发生又自愈的故障就会被完全漏掉。

各链扫块间隔不同（Tron/xlayer 3 秒，BSC/Base/Polygon/Solana 5 秒，Ethereum 12 秒），
以最快的 3 秒链算：

| `samples` | 覆盖时长（3 秒链） | 5 分钟轮询是否有盲区 | 90% 阈值需失败次数 |
|---|---|---|---|
| `20`（默认） | 60 秒 | **有，240 秒盲区** | 3 次（约 9 秒） |
| `100`（推荐） | 300 秒 | 无 | 11 次（约 33 秒） |
| `window=total` | 50 分钟 | 无 | 101 次（约 5 分钟） |

所以 `samples=100` 是这个轮询间隔下的正确选择：窗口恰好覆盖 5 分钟，
不漏事件；同时只要连续失败 11 次（约 33 秒）就会告警，比 `total` 快一个数量级。

慢链（如 Ethereum 12 秒）用 `samples=100` 覆盖 20 分钟，比需要的更宽，
只是告警稍慢（约 132 秒），不影响正确性。

### 为什么不要用 `window=total`

`total` 是 1000 个样本的长期平均值。90% 阈值下要连续失败 101 次才跌破——
失败到第 91 次时还是 90.9%，不触发。3 秒链上要 5 分钟，Ethereum 上要 20 分钟。

它的用途是看「这个节点长期健康度如何」，不适合做故障告警。

### 为什么不要关掉 `stale`

保持默认的 `stale=300` 有用：它能兜住一种成功率抓不到的情况——扫块器卡死但没有
产生失败记录（比如某次 RPC 调用挂住）。这时成功率不变，但 `stale_seconds` 会一直涨。

这个维度只在 `scanning: true` 时参与判断，网关空闲时不会误报，留着没有副作用。

---

---

## 排查：阈值好像没生效

**先看 `summary` 里的回显。** 这几个字段是服务端**实际使用**的值。
你设了 `threshold=90&samples=100`，它就该显示：

```json
"summary": { "rate_threshold": 90, "recent_samples": 100, "window": "recent" }
```

如果 `rate_threshold` 显示 `50`、`recent_samples` 显示 `20`（都是默认值），
说明你的参数根本没到服务端。

### 最常见的原因：URL 里的 `&` 被转义成了 `&amp;`

从网页、Markdown、或某些面板的输入框里复制 URL 时，`&` 可能被转成 `&amp;`。
这时参数名会变成 `amp;threshold`，服务端取不到 `threshold`，就静默用了默认的 50。

**表现完全符合「设了 90 却按 50 告警」。** 检查 changedetection 里保存的 URL，
确认分隔符是单个 `&`，不是 `&amp;`。

### 第二个判断依据：看告警文本里的阈值

告警消息会把实际阈值写进去：

```json
"alerts": ["bsc: 成功率 45.00% 低于阈值 50.00%"]
```

如果你设了 90，而这里写的是 `低于阈值 50.00%`，那就确定是参数没送到。
写的是 `低于阈值 90.00%` 则参数正常，慢是窗口造成的（见上面的窗口对照表）。

### 参数写错会明确回报

现在非法参数不再静默忽略，而是在 `summary.param_errors` 里列出来：

```json
"summary": {
  "rate_threshold": 50,
  "param_errors": ["threshold=\"abc\" 无法解析为数值，已按默认 50 处理"]
}
```

看到这个字段就说明参数有问题。支持的范围：`threshold` 为 0~100 的数值，
`stale` 为非负整数，`window` 只接受 `recent` 或 `total`。

### 想知道到底是哪个数在跟阈值比

每条网络里有 `rate_used` 字段，就是实际参与比较的那个成功率——它等于
`recent_rate` 还是 `success_rate`，取决于 `window` 参数：

```json
{
  "network": "bsc",
  "success_rate": 95.5,   // 全窗口 1000 样本
  "recent_rate": 85,      // 近 20 样本
  "rate_used": 85,        // 实际比较用的（此处 window=recent）
  "alert": true
}
```

---

## 令牌的三条性质

1. **持久**：存在数据库 `monitor_auth_token`，不会过期，容器重启不失效。
2. **独立**：与后台登录令牌分开。后台登录、改密码、退出登录都不影响它。
3. **只读**：这个接口只返回状态数据，不含任何 RPC 端点地址和 API Key，可以
   放心挂到 changedetection，不会把凭据留在它的历史快照里。

