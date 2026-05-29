---
name: Bug report
about: Something doesn't work the way the docs say it should
labels: bug
---

## What happened

<!-- One sentence. Then a few more if needed. -->

## What I expected

<!-- One sentence. -->

## Repro

<!-- Exact commands or actions. Include any non-default env vars. -->

## Environment

- macOS version: `sw_vers -productVersion` →
- tmux version: `tmux -V` →
- zdev commit / version: `zdev --version` (or `cd <repo> && sl id`) →
- Go version (if building from source): `go version` →

## Logs

<details>
<summary>~/Library/Logs/zdev/zdevd.err.log (last 50 lines)</summary>

```
paste here
```
</details>

<details>
<summary>launchctl print gui/$(id -u)/com.zdev.zdevd | head -30</summary>

```
paste here
```
</details>
