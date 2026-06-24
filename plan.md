<div align="center">

# 🗺️ devbox — What's Next (PRD)

![status](https://img.shields.io/badge/status-%F0%9F%9A%80%20P1%20shipped%20%C2%B7%20P2%20next-blueviolet?style=for-the-badge)
![base](https://img.shields.io/badge/builds_on-v1%20hardened%20%C2%B7%20v2%20M8%20shipped-00ADD8?style=for-the-badge)
![release](https://img.shields.io/badge/release-%F0%9F%93%A6%20latest%20live-success?style=for-the-badge)
![public](https://img.shields.io/badge/github-%F0%9F%8C%8D%20public%20%C2%B7%20AGPLv3-181717?style=for-the-badge)

</div>

> 📋 A short, honest backlog for what comes **after** the adoption push (installers + Docker + dashboard).
> Ordered by leverage, not by milestone number. Every item lists why it matters, its scope, and how we'll
> know it's done. M9–M11 stay **demand-driven** — captured, not committed.

## 🎯 Where we are
v1 is complete + audited + hardened. **M8 (v2 foundations) shipped & fleet-verified**: schema migration runner,
per-`(share,id)` snapshots, daemon control socket + `pause`/`resume`, **M8a teams** (principals · roles · invites
with attenuation · members), and `restore` byte-safety. Plus an **embedded live dashboard**, **cross-platform
installers** with keep-alive services, a **hub Docker image** for NAS, and **macOS Full Disk Access** detection.

**🚀 P1 shipped (2026-06-23):** real published release (`latest`, 6 OS/arch incl. **linux/arm64** for Pi),
a one-shot `scripts/release.sh`, CI on Forgejo + GitHub Actions (vet · build · `-race`), the hub image in the
Forgejo registry, and a public **`shoemoney/devbox-dist`** repo so `curl|sh` works **with no token**. The
**GitHub repo is now public** (AGPLv3, module path `github.com/shoemoney/devbox` → `go install` works) after a
clean two-scanner git-history secret sweep. Verified end-to-end on clean no-Go amd64 **and arm64** boxes.

```mermaid
flowchart LR
    NOW["✅ adoption tooling<br/>installers · docker · dashboard"] --> P1["✅ P1<br/>Releases + CI<br/>+ public repo"]
    P1 --> P2["🔨 P2<br/>M8a auth audit"]
    P2 --> P3["🥉 P3<br/>dashboard depth"]
    P3 -.demand-driven.-> LATER["🔮 M9–M11<br/>E2E · P2P · HA · TUI"]
    style NOW fill:#1e5a2e,stroke:#51cf66,color:#fff
    style P1 fill:#1e5a2e,stroke:#51cf66,color:#fff
    style P2 fill:#5a4a1e,stroke:#ffd43b,color:#fff
    style P3 fill:#1e3a5a,stroke:#4F9CF9,color:#fff
    style LATER fill:#0d1117,stroke:#4F9CF9,color:#fff
```

---

## ✅ P1 — Releases + CI (close the adoption loop) — **SHIPPED 2026-06-23**

**Why:** the `curl | sh` installer and Docker image were built, but the installer's *primary* path —
download a prebuilt binary — **had nothing to download** (no published release), so it silently fell back to
`go build` / local `dist/`. A stranger couldn't adopt devbox without Go + the repo. P1 closed that gap and gave
the v2 codebase the **regression safety** it lacked.

**Shipped**
- [x] `scripts/release.sh` — one-shot: cross-build (now **6 targets incl. `linux/arm64` + `linux/arm`** for Pi) → create/update the release → upload **de-versioned** assets (`devbox_<os>_<arch>`, `+.exe`) the installer downloads directly, plus a de-versioned `SHA256SUMS` so `shasum -c` matches. Idempotent.
- [x] CI on **Forgejo Actions** (`.forgejo/workflows/`) **+ GitHub Actions** mirror: `go vet` · `go build ./...` · `go test ./... -race` on push/PR; release-on-tag workflow ready. **GitHub CI is green on every push** (Forgejo waits on a self-hosted runner — per the gotcha, the local `release.sh` is the real path).
- [x] Hub image published to the Forgejo registry: `git.shoemoney.ai/shoemoney/devbox-hub:latest`; compose now `image:` by default (`--build` for local source).
- [x] **Deviation (approved):** the source repo stays private, so a public **`shoemoney/devbox-dist`** repo carries the installers + binaries as release assets → `curl|sh` works **with no token**. Separately, the **GitHub mirror was made public** (AGPLv3 — matching the open-core moat, module path `github.com/shoemoney/devbox` so `go install …/cmd/devbox@latest` works) after a clean two-method git-history secret scan.

**Acceptance — all met ✅**
- ✅ Clean **no-Go** box, `curl -fsSL …/install.sh | sh` installs a working `devbox` from the real release — verified end-to-end on **amd64** and on **arm64** (qemu Pi-proxy; binary runs). Real Pi pending its next wake.
- ✅ `docker compose up -d` pulls the published image (no local build) — verified on the NAS (throwaway stack, production hub untouched).
- ✅ CI green on `main` (GitHub Actions) and gates PRs.

**Effort:** M · **Risk:** low — landed without product-code changes (module-path rename only).

---

## 🥈 P2 — Adversarial audit of the M8a auth surface

**Why:** M8a added real **privilege** code — invite **attenuation** (`meta.MayGrant`), the push **write-gate**,
principal binding on join, token handling. It has unit + HTTP + fleet tests, but no dedicated *adversarial* pass.
This is the M7.5 treatment for v2's new attack surface, **before** anyone relies on it for multi-owner shares.

**Scope (hunt for, with regression tests for each real finding)**
- [ ] **Invite replay / reuse** — can a redeemed invite token be used twice? (`tokens.used` + the binding.)
- [ ] **Privilege escalation** — any path where `MayGrant` is bypassed; self-invite to a higher role; granting `+s` you don't hold; touching a principal who outranks you.
- [ ] **TOCTOU** on the legacy→explicit flip + self-seed (concurrent first grants; `publishMu` coverage).
- [ ] **Revoked-device bearer reuse** + whether revocation actually closes write access immediately.
- [ ] **Cross-share leakage** — does a member grant on share A ever imply rights on share B?
- [ ] Join PoP edge cases under the new binding path.

**Acceptance**
- ✅ Each confirmed finding has a failing-then-passing regression test; `-race` clean; fleet-verified where it matters.
- ✅ A short `docs/M8a-audit.md` recording findings + the residual single-owner-threat-model deferrals.

**Effort:** M · **Risk:** medium (security-sensitive — do it carefully, don't rush).

---

## 🥉 P3 — Dashboard depth (delight, not load-bearing)

**Why:** the live dashboard wows already, but it only animates `join` + `push`. More flow types + a terminal
view would make it a genuinely complete ops surface.

**Scope**
- [ ] Emit `pull` (head fetch / propagation), `gc` (sweep), and `conflict` flow events from the hub; animate them.
- [ ] A historical sparkline window persisted server-side (currently the frontend computes rates from the live stream only).
- [ ] *(Optional)* an **M11 TUI** over the daemon control socket (`devbox top`?) — re-adds the `/events` SSE stream we trimmed, now with a real consumer (bubbletea).

**Acceptance**
- ✅ A `gc` run and a multi-device `pull` both visibly animate; new event types fleet-verified on the SSE stream.

**Effort:** S–M · **Risk:** low.

---

## 🔮 Deferred — demand-driven (per the v2 spec's own guard)

Do **not** pre-build these — there's no user who needs them yet, and the spec explicitly defers them:

| Milestone | Trigger to build it |
|---|---|
| **M9 — E2E (convergent encryption)** | a real **untrusted-hub** user appears (you self-host on your own NAS → you trust it) |
| **M9 — S3/R2 + Litestream HA** | hub durability/DR becomes a felt need beyond NAS snapshots (slots behind the `blobstore.Store` seam) |
| **M9 — read-side ACL gating** | a genuinely untrusted **multi-owner** share exists |
| **M10 — LAN P2P chunk exchange** | the hub uplink actually hurts (today the hub is *on* the LAN) |
| **M10 — conflict sidecar + diff3 resolver** | conflicts get frequent enough to want interactive 3-way merge |
| **M11 — full TUI + power sanity** | on demand |

---

<div align="center">

**Recommendation:** start at **P1**. It finishes the adoption story you just set and hardens the build —
highest leverage, lowest risk. 🚀

</div>
