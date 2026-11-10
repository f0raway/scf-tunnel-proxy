# 部署、升级与日常运维

## 通用部署目录

| 路径 | 内容 |
|---|---|
| `/opt/scf-tunnel-proxy/scf-tunnel` | 当前入口程序 |
| `/etc/scf-tunnel-proxy/agent.json` | 代理认证、节点令牌、节点清单 |
| `/var/lib/scf-tunnel-proxy/usage.json` | 费用估算账本，重启/升级时必须保留 |
| `/var/log/scf-tunnel-proxy/service.log` | 连接元数据、错误和启动日志 |
| `/etc/systemd/system/scf-tunnel-proxy.service` | 主服务 |
| `/etc/systemd/system/scf-tunnel-proxy-watchdog.timer` | 每两分钟本地检查 |

默认服务用户为 `scftunnel`。配置由 root 持有、服务组可读；数据目录仅服务用户可访问。运行时不读取腾讯云管理凭据。

## 首次部署顺序

1. 在持有腾讯云部署权限的可信机器构建 Linux amd64 二进制。
2. 复制 `.example` 配置，设置凭据环境变量，执行 `--dry-run` 检查包格式和计划。
3. 明确资源和费用后执行 `deploy/deploy.py`，保留令牌与部署状态。
4. 将二进制、`ops/` 和私有 `agent.json` 安全送到入口服务器。
5. 运行安装脚本，只向可信客户端开放代理端口；面板保留回环监听。
6. 实际测试正确/错误认证、HTTP、HTTPS、SOCKS5、目标不可达、连接复用及预期并发，再投入使用。

云端程序通过可执行 `scf_bootstrap` 运行。依赖 Python3.9 基础运行环境不代表用 Python 解释 Go 二进制，它只是 SCF 的基础环境；实际执行的是静态 Linux 程序。

## 更新云端

```bash
bash ops/build.sh
.venv/bin/python deploy/deploy.py --update
```

工具只更新自己标记为 `scf-tunnel-proxy managed:v1` 的函数；同名但不属于本工具的函数会拒绝覆盖。既有云端令牌与本地令牌不一致时也拒绝更新，避免把节点更新成入口无法认证的状态。

先更新节点，再更新入口；协议 2 节点兼容旧入口的有效连接。发布过程和平台更新可能影响长连接，应选择合适维护窗口。不要假设更新对既有会话完全无影响。

部分区域失败时，脚本退出非零并保留结果，不覆盖入口配置，但此前成功创建/更新的云资源仍存在；不会自动回滚或删除它们。排查后重新执行，必要时在控制台按名称确认并清理。

## 更新入口

先检查现有 `agent.json` 中账号、令牌、节点和账本路径，备份当前配置、数据和旧二进制，然后：

```bash
sudo bash ops/install.sh dist/scf-tunnel-linux-amd64 runtime/agent.json
```

该脚本不会清空账本。正常停止会关闭活跃隧道并结算，更新会使客户端需要重连。若需要同时修改节点清单，先验证云端新节点可用，再更新入口配置。

安装脚本面向通用路径，不会自动迁移本次历史部署；见 [迁移说明](EXISTING_DEPLOYMENT.md)。

## 启停与日志

```bash
sudo systemctl status scf-tunnel-proxy --no-pager
sudo systemctl restart scf-tunnel-proxy
sudo tail -100 /var/log/scf-tunnel-proxy/service.log
curl -s http://127.0.0.1:8081/api/stats
sudo cat /var/lib/scf-tunnel-proxy/usage.json
```

主服务异常退出后约 10 秒重启；正常管理员停止不会被主服务自身拉起，但健康检查仍可能恢复它。**停用时同时关闭健康检查**：

```bash
sudo systemctl disable --now scf-tunnel-proxy-watchdog.timer scf-tunnel-proxy
```

恢复：

```bash
sudo systemctl enable --now scf-tunnel-proxy scf-tunnel-proxy-watchdog.timer
```

本地检查每两分钟访问回环面板并检查入口端口，连续两次异常才重启。不通过云端访问第三方网站，因此不会周期性创建收费隧道；它也不能证明某个目标站点可达。

日志由系统 logrotate 检查和轮转，配置为 `daily/maxsize 5M/rotate 5`；`maxsize` 只在 logrotate 实际执行时判断，**不代表日志实时硬上限 5M**。重复 SOCKS 错误每分钟采样并汇总，以降低日志洪泛。

## 备份与恢复

备份必须包括：运行配置、账本、旧二进制、部署用节点令牌及必要管理配置。备份包含敏感信息，权限应为 600，保存在可信位置。

恢复时使用相互匹配的节点令牌、节点 URL 与云函数环境变量。不要为了恢复连接删除费用账本；也不要让旧进程与新进程同时写同一个账本。历史月份和活跃连接的估算不是精确的云结算金额。

安装脚本在已有通用部署上更新前，会生成 root 专属备份到 `/var/backups/`。它不自动保证任何失败场景都能回滚：如新程序无法运行，停止健康检查、恢复旧二进制/配置并重新启动主服务，随后验证转发。

## 密钥与成本

- 腾讯云管理凭据只放在部署环境或权限 600 的本地配置，不给入口进程。
- 节点令牌只放在入口私有配置和 SCF 环境变量；不要把真实节点清单或带密码的配置提交 Git。
- 代理入口必须限制网络可达范围。HTTP/SOCKS5 入口认证没有独立传输加密，应由可信内网或 VPN 保护。
- 需要轮换节点令牌时，计划同步更新入口和所有节点环境，并验证。当前部署工具故意不自动轮换已有令牌。
- 云账号可另配置费用提醒；本项目没有自动创建账单告警，也不代替腾讯云账单。
