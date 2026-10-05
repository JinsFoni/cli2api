### English

- Move Qoder check-in and quota refresh into the Go process: daily credit claims now run over direct Bearer HTTP with the stored credential, and the browser-grade worker stays chat-only. Accounts kept enabled just for check-in can now be disabled, which frees the several-hundred-MB worker process per account. Refreshed tokens are written back to the credential store instead of being lost.

### 中文

- Qoder 签到与额度刷新迁入 Go 进程：每日积分领取改为携带存储凭证的直连 Bearer HTTP，Node worker 只保留聊天推理。仅为签到保留的账号现在可以直接停用，每个账号可省下数百 MB 的常驻 worker 内存。刷新后的令牌会写回凭证库，不再丢失。
