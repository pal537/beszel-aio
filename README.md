# Beszel 系统监控（fnOS 移植版）

轻量级服务器监控工具 [Beszel](https://github.com/henrygd/beszel) 的飞牛 fnOS 原生移植（非 Docker），由 **stingeo** 打包发布。Beszel 本体未作任何修改，移植层只负责它在 fnOS 上的安装、启停与桌面窗口适配。

## 功能（Beszel 原版）

- 系统监控：CPU、内存、磁盘、网络、温度、风扇、负载的历史曲线
- Docker 容器监控：逐容器的 CPU / 内存 / 网络流量
- 硬盘 S.M.A.R.T. 与存储池状态
- systemd 服务状态一览
- 网络监控：从本机对任意目标做 ICMP / TCP / HTTP 延迟与丢包监测（HTTP 目标附带证书到期信息）
- 多机监控：一台面板汇总多台机器，各机安装 beszel-agent 回连即可
- 阈值告警与通知

## 本移植版做的适配

- hub 与 agent 同包装入：安装后自动完成本机配对，第一台被监控机器就是本机，免注册、免配置、开箱即用
- 在飞牛桌面窗口内直接打开：自动复用飞牛证书库出 HTTPS，并按访问域名匹配证书
- 默认自动登录进面板；本地管理员账号随机生成，可在应用设置页查看、修改，也可关闭自动登录
- 数据只保存在本机应用数据目录，不外传

## 安装

1. 在 [Releases](https://github.com/pal537/beszel-aio/releases) 下载 `beszel-aio-0.21.0-fnos.fpk`
2. 飞牛应用中心 → 手动安装，选择该 fpk 文件
3. 安装完成，从飞牛桌面打开「Beszel 系统监控」即用

也可以添加 FnDepot V2 应用源，源文件为本仓库根目录的 [`fnpack.json`](fnpack.json) 直链：
`https://raw.githubusercontent.com/pal537/beszel-aio/main/fnpack.json`

## 说明

- 平台：飞牛 fnOS（x86_64）
- 端口：18092 / tcp
- 权限：以 root 运行——agent 读取 S.M.A.R.T.、Docker 与 systemd 信息需要
- 若飞牛未配置任何证书，服务将回退为 HTTP，此时请用新标签页方式打开（桌面窗口内嵌会被浏览器拦截）

## 构建

```bash
./build.sh
```

脚本会：从 Beszel 官方 Release 下载 v0.21.0 二进制并校验 SHA-256 → 用 Go 编译包装网关（gateway/）→ 用 fnpack 打包出 fpk。

## 出处与许可

- Beszel © [henrygd](https://github.com/henrygd)，MIT License，上游仓库：https://github.com/henrygd/beszel
- 本仓库是 fnOS 打包移植，发布者 **stingeo**；包装网关源码（gateway/）同以 MIT 发布
- 仓库根目录 LICENSE 为上游 Beszel 的 MIT 许可证全文
