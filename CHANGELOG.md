# Changelog

本项目所有重要变更都记录于此。
格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [1.2.0] - 2026-10-18

### 新增
- **控制台改用 cli2api 前端**：直接复用 CLI2API 的 React + HeroUI 界面构建产物，
  外观与操作与原项目一致；新增 `internal/cliui` 兼容层，按 cli2api 的 HTTP 契约
  输出本服务的真实数据（账号池、模型、接入参数、运行时）
- 前端工程并入仓库 `frontend/`，`npm run build` 后将产物同步到
  `internal/webui/static/` 并由 `go:embed` 内嵌，保持单二进制交付
- 控制台路由：`/` 为 SPA（含 `/login` `/accounts` `/providers` `/access` `/logs` `/keys` `/system`），
  `/api/*` 为兼容接口，原有 `/admin/*` 面板保留
- 未配置 `api_key` 时自动预填占位密钥，控制台不再卡在登录页

### 说明
- 前端基于 [cli2api](https://github.com/caigee-cmd/cli2api)（MIT License，Copyright (c) 2026 caigee-cmd），
  许可声明保留于 `frontend/LICENSE`
- Providers / Keys / 请求日志等页面对应后端能力本服务尚未提供（多渠道、多密钥、请求日志存储），
  接口返回空集以保证界面可用；模型列表与账号操作均为真实数据

## [1.1.0] - 2026-10-18

### 新增
- 控制面板全面改版：侧边栏分组导航（概览 / 账号 / 接入 / 签到 / 系统）、明暗主题切换、统一卡片与状态标签
- **面板内添加账号**：一键生成 PKCE 授权链接，浏览器完成授权后粘粘 code 即可；自动换取 token 与 api_key 并原子写入 `auths/`，新账号立即入池生效
- 账号页支持搜索、状态过滤、单账号签到 / 积分查询、全部解禁、账号级代理测试

### 修复
- **修复面板卡死**：`CreateAPIKey` 在持有账号写锁时又去读代理，`sync.RWMutex` 不可重入导致自锁死锁（新增 `doLocked` / `doLockedControl`，持锁路径不再重复取锁）
- **修复账号重启后失效**：token 刷新成功后立即原子落盘到 `auths/`，不再依赖调用方记得保存
- **修复面板查询卡顿**：控制面请求（签到 / 积分 / profile / 刷新 / 建 key）改用独立短超时（默认 20s，可用 `P2A_CONTROL_TIMEOUT` 调整），流式对话仍用长超时

### 变更
- 凭证刷新改为“先快照、锁外发请求”，避免长时间持锁阻塞其它请求

## [1.0.1] - 2026-10-16

### 修复
- 多架构（amd64/arm64）Docker 镜像构建改为原生交叉编译，不再依赖 QEMU 模拟，构建更稳定
- 修正文档与发布脚本中的仓库地址引用

## [1.0.0] - 2026-10-15

### 新增
- 账号安全加固：按账号隔离代理、并发限制、每日限额、请求前随机抖动
- 每日自动签到领积分
- 管理控制台（账号池、密钥、日志查看）
- 一键发版脚本与 GitHub Actions 自动构建（多平台二进制 + Docker 镜像）
- 反向隧道支持，便于内网部署
