# DISC Podcast & Radio (播客与网络收音机)

SNOWSKY / 飞傲 DISC 便携式 Linux 音乐播放器原生扩展组件与 UI Hook 模块。

---

## ⚡ 开发者与 AI 接力必读

如果你是接力开发的大模型 (AI) 或开发者，**在修改代码前请务必先查阅核心档案**：
- **[系统架构与 AI 接力全景技术手册](../AI_HANDOVER.md)**：包含君正 X2000 MIPS 架构规范、Flash 自动擦除致命陷阱解析、LVGL 逆向内存地址映射及接力守则。
- **[全版本详细技术更新历史 (CHANGELOG)](../CHANGELOG.md)**：包含从 v1.0.0 到最新版本的所有功能演进、致命 Bug 逆向排查与底层修复原理。

---

## 目录结构

- `disc-ui-hook/`：UI Hook C 语言源码 (`disc_ui_hook.c`) 与预检工具 (`test_loader.c`)。
- `disc-podcast-radio/`：Go 编写的核心服务后台，包含 Web 前端控制面板 (`web/`)、流媒体解析代理、播放列表管理、IPC 通信与开机启动脚本 (`entry.sh`)。

---

## 一键自动化构建与发版

发版脚本位于 `../build_release.py`，支持自动递增版本号 (+0.0.1)、同步宏定义、Clang MIPS 编译、Go 交叉编译、固件规范校验、更新日志聚合与生成发布压缩包：

```powershell
# 每次调整代码后发版（版本自动自增 0.0.1）
python e:\DISC\_内部\build_release.py

# 仅更新文档或重新打包（不递增版本）
python e:\DISC\_内部\build_release.py --no-bump
```
