# DISC Podcast & Radio

DISC 播放器网络播客与电台功能组件及 UI Hook 模块。

## 目录结构

- `disc-podcast-radio/`：Go 编写的网络电台/播客核心服务，包含 Web 前端控制面板 (`web/`)、流媒体解析代理、播放列表管理及 IPC 通信。
- `disc-ui-hook/`：UI Hook C 语言源码与交叉编译脚本 (`mipsel-linux-gnu`)，用于注入与加载 UI 模块。

## 构建与运行

### disc-podcast-radio (Go)
```bash
cd disc-podcast-radio
go build -o disc-podcast-radio .
```

### disc-ui-hook (C & Python)
```bash
cd disc-ui-hook
python build_ui_hook.py
```
