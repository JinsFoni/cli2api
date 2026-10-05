### English
- Qoder accounts now run fully in-process: chat, login, model catalog,
  check-in, and usage no longer spawn a per-account Node worker.
- Multiple Qoder accounts no longer cost hundreds of MB each; memory usage
  with many accounts drops to near zero for idle accounts.
- Node.js and the qodercli / qoderclicn packages are no longer required at
  runtime — the Docker image no longer ships a Node layer. SOCKS proxies are
  now supported for Qoder accounts.
- Qoder global accounts can now claim the daily 100 Credits check-in — the
  campaigns/claim flow is identical across regions (verified live against
  openapi.qoder.sh).

### 中文
- Qoder 账号改为全进程内原生实现:chat、登录、模型目录、签到与用量不再
  为每个账号拉起 Node worker。
- 多账号场景下,空闲账号内存占用接近零,不再每个账号消耗数百 MB。
- 运行时不再需要 Node.js 与 qodercli / qoderclicn 包,Docker 镜像也不再
  包含 Node 层;Qoder 账号现支持 SOCKS 代理。
- Qoder 国际版账号现支持每日 100 Credits 签到 —— 两个区域走同一套
  campaigns/claim 流程(已在 openapi.qoder.sh 实测验证)。
