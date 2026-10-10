### English

- Qoder check-in: a missing runtime-info helper now reports "device identity unavailable (…)" instead of the misleading "check-in activity not open". Set `QODER_INSTALL_RUNTIME_INFO=1` to let the server self-install the helper on Linux amd64/arm64 — it is extracted from the official npm package `@qoder-ai/qodercli` after a sha512 integrity check and stored under `QODER_DATA_DIR/.bin/`. Desktop-app helpers always take precedence.

### 中文

- Qoder 签到：缺少 runtime-info 助手时如实报「缺少设备风控身份（…）」，不再误报「签到活动未开放」。设置 `QODER_INSTALL_RUNTIME_INFO=1` 可在 Linux amd64/arm64 上自动安装助手：从官方 npm 包 `@qoder-ai/qodercli` 提取（先做 sha512 校验），落盘在 `QODER_DATA_DIR/.bin/`。桌面客户端自带的助手始终优先。
