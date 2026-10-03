# Contributing to NexBoot

**English** · [简体中文](#参与贡献简体中文)

Thanks for helping. Bug reports, fixes and docs are all welcome.

## Before you open an issue

- Search existing issues first.
- For boot problems, include: client firmware (BIOS or UEFI), NIC model, image OS and build, and what the client screen shows.
- Attach the server log: `journalctl -u ndiskless --since "-30min"`.

## Pull requests

Use the Go version in `go.mod` and Node.js 22 (at least 22.22.2) or 24 (at least 24.15.0), with npm. The Makefile builds and embeds the frontend automatically; do not commit `web/dist` or other generated files.

1. Open an issue first for anything larger than a small fix, so we can agree on the approach.
2. Keep each PR to one change.
3. Write a failing test first, then the fix.
4. Run the same checks as CI before you push:

   ```sh
   make verify-all   # gofmt, vet, Go tests, static build, frontend tests/build
   ```

5. Code comments are written in Chinese and kept short: say why, not what.

Features that need real ZFS, LIO or several machines are covered by the end-to-end matrices in [`tools/e2e/`](tools/e2e/README.md). Mention in the PR whether you ran them.

By contributing you agree that your work is licensed under [AGPL-3.0](LICENSE).

---

## 参与贡献（简体中文）

欢迎报告问题、修复缺陷、改进文档。

**提 Issue 前**

- 先搜一下有没有相同的问题。
- 开机问题请写明：客户机固件（BIOS / UEFI）、网卡型号、镜像系统和版本号、客户机屏幕上的报错。
- 附上服务端日志：`journalctl -u ndiskless --since "-30min"`。

**提 Pull Request**

使用 `go.mod` 指定的 Go 版本，以及 Node.js 22（至少 22.22.2）或 24（至少 24.15.0）与 npm。Makefile 会自动构建并嵌入前端；不要提交 `web/dist` 等生成产物。

1. 改动较大时先开 Issue，商量好做法再动手。
2. 每个 PR 只做一件事。
3. 先写会失败的测试，再写修复。
4. 推送前跑和 CI 相同的检查：`make verify-all`。
5. 代码注释用中文，写清「为什么」，一般一两行。

需要真实 ZFS、LIO 或多台机器的功能，由 [`tools/e2e/`](tools/e2e/README.md) 里的端到端矩阵覆盖。请在 PR 里说明是否跑过。

提交贡献即表示你同意以 [AGPL-3.0](LICENSE) 授权你的代码。
