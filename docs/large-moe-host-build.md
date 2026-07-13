# Decision Doc — Large-MoE Host Build (the "bigger than the Sparks" box)

**Status:** Scoped · **Date:** 2026-06-12 (scope locked 2026-06-13; K2.7-Code added 2026-06-14) · **Owner:** Steven

## Scope (locked)

- **Primary target: DeepSeek-671B-A37B class** (also Qwen3-Coder-480B-A35B) at
  Q4 → **fits in 576 GB**. *Not* optimizing for Kimi-K2-at-Q4 (would force
  768 GB); leave that as optional headroom.
- **Capacity target: 576 GB**, all 12 DDR5 channels populated (12×48 GB).
- **Speed: ~5–8 t/s acceptable, 15+ t/s ideal** (some models are interactive).
  This tilts toward Turin-class bandwidth where budget allows.
- Two builds spelled out below: **Value (Genoa)** and **No-compromise (Turin)**,
  with a shared upgrade path between them.

## Purpose

Spec a new node to run MoE models **too large to fit on the two GB10 Sparks even
combined (> 256 GB)** — at a *usable* decode speed, for the best bang-for-buck,
on an **AMD CPU**, while respecting the 2026 DRAM price surge. This is the
"giant model" host; hekaton (the 1.5 TB Ivy-Bridge box) was a novelty and is
hard-capped by its AVX1-only CPU (see "Why not just use hekaton").

## TL;DR recommendation

**Single-socket EPYC Genoa (Zen4) or Turin (Zen5), all 12 DDR5 channels
populated (~576 GB via 12×48 GB), one 48 GB NVIDIA GPU, running
`ik_llama.cpp` with experts on CPU + attention/KV/shared on GPU.**

- That's the value sweet spot: AVX-512 (unlocks `ik_llama.cpp`'s fast IQK
  kernels), ~440–576 GB/s RAM bandwidth, PCIe 5.0, fits everything up to
  DeepSeek-671B at Q4 with room for context.
- Expect **~10–15 tok/s decode** on a 671B-A37B model in hybrid mode; ~20+ on
  Turin. Prefill is much faster (GPU does attention).
- **The single biggest cost lever is RAM, and the #1 config mistake is
  under-populating channels.** Fill all 12 DDR5 channels — 12×48 GB at full
  bandwidth beats 8×64 GB (same-ish capacity, but only 8/12 of the bandwidth →
  ~1/3 slower decode).
- Go **NVIDIA** on the GPU even though the CPU is AMD: the fast hybrid engines
  (`ik_llama.cpp`, KTransformers, vLLM, SGLang) are CUDA-first. (On hekaton the
  GPU vendor was moot because AVX1 blocked those engines anyway — not so here.)

The open decisions (budget, which models matter most, acceptable speed) are at
the end; they pick the *tier*, not the architecture.

---

## The one principle that drives everything

For a big MoE in CPU RAM, **decode is memory-bandwidth-bound**, not
compute-bound. Per generated token you stream the *active* expert weights from
RAM once:

```
decode tok/s  ≈  (usable RAM bandwidth)  /  (active-param bytes per token)
              ×  ~0.4–0.7 efficiency (NUMA, latency, kernel)
```

Example — DeepSeek 671B-**A37B** at Q4 (~18.5 GB of active weights/token):

| Platform | channels × DDR | ~usable BW | theoretical ceiling | realistic decode |
|---|---|---|---|---|
| Ryzen 9950X | 2 × DDR5 | ~90 GB/s | ~5 t/s | ~1–2 t/s |
| EPYC Milan / Rome | 8 × DDR4-3200 | ~190 GB/s | ~10 t/s | ~3–4 t/s (CPU) |
| Threadripper PRO | 8 × DDR5 | ~225–330 GB/s | ~13–18 t/s | ~6–9 t/s |
| **EPYC Genoa** | **12 × DDR5-4800** | **~440 GB/s** | **~22 t/s** | **~8–15 t/s (hybrid)** |
| **EPYC Turin** | **12 × DDR5-6000** | **~570 GB/s** | **~28 t/s** | **~15–22 t/s (hybrid)** |

Three corollaries:
1. **Channel count = decode speed.** Two-channel anything (Ryzen) is a non-starter
   for models this size. EPYC's 12 channels is the whole point.
2. **A GPU helps by *removing bytes from the RAM path*** — it holds attention,
   KV cache, dense/shared layers, and as many hot experts as VRAM allows, so the
   CPU streams fewer expert bytes/token. More VRAM → more experts resident →
   faster decode. PCIe 5.0 (EPYC) makes this offload ~2–4× more effective than
   hekaton's PCIe 3.0.
3. **AVX-512 unlocks the fast software** (next section). Without it you're on
   plain `llama.cpp`/AVX2 and leave ~30–50% on the table.

---

## Software stack (and why it dictates AVX-512 + NVIDIA)

| Engine | Runs on AMD AVX-512? | Hybrid expert offload | MTP / spec-decode | Verdict for this box |
|---|---|---|---|---|
| **ik_llama.cpp** | **Yes** (IQK kernels target Zen4/Zen5) | **Yes** (`-ot exps=CPU`, `-fmoe`, `-mla`) | partial | **Primary engine on AMD** |
| llama.cpp (mainline) | Yes (AVX2 baseline; AVX-512 paths) | Yes (`--n-cpu-moe`/`-ot`) | MTP merged May 2026 | Fallback / widest model support |
| KTransformers | Degraded — **needs Intel AMX** for its fast path; AMD = AVX-512 fallback, no official AMD support | Yes | Yes (EAGLE/MTP) | **Skip on AMD** (AMX is Intel-only) |
| vLLM / SGLang | CPU not used for compute | not shipped / not designed | Yes (DeepSeek) | Full-GPU only — not for CPU-RAM experts |

Key facts:
- **AVX-512 arrived with Zen4 (Genoa).** Milan/Rome (Zen2/3) have **no AVX-512**
  → `ik_llama.cpp` falls back to AVX2. Zen5 (Turin) has *native full-width*
  512-bit (Zen4 double-pumps), ~2× the AVX-512 kernel throughput — though decode
  is so bandwidth-bound this mostly matters for prefill.
- **KTransformers' headline numbers are Intel AMX** (Sapphire/Emerald Rapids).
  AMD has no AMX. So the "27× faster than llama.cpp" KTransformers story does
  **not** transfer to an AMD box — use `ik_llama.cpp` instead, which is the
  best CPU MoE engine on AMD AVX-512.
- The fast engines are **CUDA-first** (`ik_llama.cpp` is CUDA+CPU only; ROCm/
  Vulkan "not maintained"). → **buy an NVIDIA GPU** for this box.
- **NUMA tuning is mandatory:** BIOS `NPS1`, `numactl -N 0 -m 0` to pin. And
  **single socket beats dual for decode** — dual-EPYC MoE token-gen barely
  scales (small-matrix FFN sync bottleneck: 2×9654 ≈ 105% of one socket). Spend
  on bandwidth-per-socket and RAM, not a second socket.

---

## RAM: the cost driver (2026 reality)

The DRAM surge is real and not easing before 2027 (server DRAM +45–60% QoQ
through early 2026, AI buildouts crowding out capacity). Snapshot pricing:

| | new | used/refurb |
|---|---|---|
| DDR4 ECC RDIMM | ~$9/GB | **~$4–6/GB** (plentiful) |
| DDR5 ECC RDIMM | **~$15/GB** | thin used market |

Cost to populate (illustrative, mid-2026):

| Capacity | DDR4 (used RDIMM) | DDR5 (new RDIMM) |
|---|---|---|
| 384 GB | ~$1.5–2.4k | ~$3–6k |
| 512 GB | ~$2–3.2k | ~$4–9k |
| 576 GB (12×48) | ~$2.5–3.5k | ~$5–11k |
| 768 GB | ~$2.4–4.8k | ~$8–19k |

This is the crux of the bang-for-buck tension: **used DDR4 is 3–5× cheaper per
GB**, which is why the DDR4 Milan path stays tempting despite being slower.

> **Channel-population rule (don't skip):** to get a platform's rated bandwidth
> you must populate every channel. On a 12-channel Genoa/Turin board, **12×48 GB
> = 576 GB at full ~460–576 GB/s** is materially better than 8×64 GB = 512 GB,
> which runs at only 8/12 of bandwidth (~300 GB/s) → ~1/3 slower decode for a
> bit more capacity. Choose DIMM size to fill all 12 slots.

---

## GPU choice (NVIDIA; size is the question)

Reusing the GPU comparison; for *this* box PCIe 5.0 makes offload far more
effective than on hekaton. VRAM capacity matters more than raw bandwidth here
(it sets how many experts stay off the RAM path).

| GPU | VRAM | BW | ~price (2026) | role |
|---|---|---|---|---|
| RTX 3090 (used) | 24 GB | 936 GB/s | ~$700–1,000 | entry; attention+KV+dense, few experts |
| RTX 4090 | 24 GB | 1008 GB/s | ~$2,000 | + FP8; same VRAM as 3090 |
| Quadro RTX 8000 (used) | **48 GB** | 672 GB/s | ~$2,000 | **value 48 GB**, passive (server-friendly) |
| RTX A6000 (used) | **48 GB** | 768 GB/s | ~$3,000–3,800 | 48 GB, Ampere, passive |
| RTX 6000 Ada (used) | 48 GB | 960 GB/s | ~$5,600 | 48 GB + FP8, passive |
| RTX PRO 6000 Blackwell | **96 GB** | 1790 GB/s | ~$8,500 | premium; holds a big chunk of experts |

- **24 GB**: fits attention + KV + dense; ~0 experts for a 380 GB model. Good
  prefill win, modest decode help. Fine entry point.
- **48 GB**: the practical sweet spot — holds attention/dense + a real slice of
  hot experts; passive workstation cards (RTX 8000 / A6000) fit a server chassis
  (consumer 3090/4090 axial coolers cook in racks). **Recommended.**
- **96 GB** (RTX PRO 6000 Blackwell): step-function for the very largest models;
  premium price.
- Avoid AMD/Intel GPUs *here* — the fast engines are CUDA-only on this path
  (different from hekaton, where AVX1 made vendor moot).

Real hybrid data points (`ik_llama.cpp`, DeepSeek 671B): EPYC 9334-QS + RTX 3070
→ **10.3 t/s**; TR PRO 7965WX + A6000 48 GB → **13.1 t/s** (Q2). A full
12-channel Genoa/Turin + 48 GB card should beat both.

---

## Model fit & expectations (the targets)

| Model | active | Q4 size | arch support today | MTP? | notes |
|---|---|---|---|---|---|
| DeepSeek V3/R1 671B-A37B | 37B | ~377 GB | ✅ mature (llama.cpp/ik_llama.cpp) | ✅ (vLLM/SGLang; llama.cpp May'26) | **best-supported giant; center the build here** |
| Qwen3-Coder-480B-A35B | 35B | ~270 GB | ✅ standard Qwen3 MoE | likely | well-supported |
| MiniMax M3 428B-A23B | 23B | ~240 GB | ⚠️ **not yet** — MSA + multimodal, no GGUF/arch support yet | n/a | wait for llama.cpp arch PR; lower active = faster once supported |
| Nemotron-3 Ultra 550B-A55B | 55B | ~300 GB | ⚠️ Mamba-hybrid arch support uncertain | has MTP | **A55B = compute-heavy**, slower decode even on big BW |
| **Kimi K2.7-Code** 1T-A32B | 32B | IQ4_XS ~495 / Q4_K_XL ~584 GB | ✅ GGUF day-1 (Unsloth) | — | native INT4 (QAT) → Q4 ≈ lossless, no gain above it. Near-lossless Q4 wants **768 GB**; **IQ4_XS (495 GB) fits 576**; Q2_K_XL ~339 GB, IQ1_M ~304 GB |

Takeaways: **DeepSeek-671B is the natural primary target** (mature support, MTP,
fits 512–576 GB). Active-param count drives decode — M3 (A23B) will be the
*fastest* of the giants once it has GGUF support; Nemotron Ultra (A55B) the
slowest. **Kimi K2.7-Code (released 2026-06-12) is the model that sets the
576-vs-768 GB decision:** its **A32B is *fewer* active params than DeepSeek's
A37B**, so it **decodes at a similar t/s** on this box — the 1 T total only
dictates *RAM*, not speed. At 576 GB you run it at IQ4_XS (495 GB); only 768 GB
buys near-lossless Q4 (584 GB). Because it ships **native INT4 (QAT)**, Q4 is
effectively lossless and there's no reason to go above it. (Access today:
Moonshot's API — **not yet on the Zen endpoint**, which tops out at `kimi-k2.6`.)

### Coding quality vs RAM — the buying lens

Coding-benchmark scores map onto RAM tiers, so "how good at coding" and "how much
RAM" are really the same decision. **Caveat:** vendor SWE-bench-Verified numbers
use different agent scaffolds and aren't directly comparable; the open-weights
~80% cluster is likely a *bigger* gap to Opus under a standardized harness (Scale
SEAL), not the ~7-point headline.

| Model (open weights) | SWE-bench Verified | Q4 RAM | Fits 576 GB hedge? |
|---|---|---|---|
| DeepSeek V3.2 (671B-A37B, MIT) | ~67–73% | ~380 GB | ✅ comfortable |
| Qwen3-Coder-480B-A35B (Apache-2.0) | ~65–67% | ~270 GB | ✅ comfortable |
| MiniMax M2.7 (230B, non-commercial) | ~78% *unconfirmed* (official SWE-Pro 56%) | ~140 GB | ✅ easy |
| Kimi **K2.7-Code** (1T-A32B, mod-MIT, coding-tuned) | ~80%+ (vendor; +21.8% Kimi Code Bench v2 vs K2.6) | IQ4_XS ~495 / Q4 ~584 GB | ⚠️ IQ4_XS fits; near-lossless Q4 → 768 GB |
| DeepSeek V4 Pro (1.6T-A49B, MIT) | ~80% (aggregators) | ~800 GB+ | ❌ Tier C+ (768 GB–1 TB) |
| *ref:* Claude Opus 4.7 / 4.8 | 87.6 / 88.6% (vendor, API-only) | — | not self-hostable |

The knee: the **576 GB hedge comfortably runs the ~65–73% tier** (DeepSeek
V3.2-671B, Qwen3-Coder-480B) — very good, self-hosted, ~10–20 *effective* points
behind Opus. Chasing the **~80% open tier costs RAM** — Kimi K2.7-Code runs at
IQ4_XS (495 GB) on 576 GB but wants **768 GB** for near-lossless Q4, and DeepSeek
V4 Pro (1.6 T) is squarely 768 GB–1 TB. **No
open model matches Opus 4.7/4.8 for hard agentic coding yet**; the choice is
"very good + owned" vs "best + rented."

---

## Aside — why not Intel (AMX)?

Intel's **AMX** (Advanced Matrix Extensions, on Xeon Sapphire / Emerald /
Granite Rapids) is a dedicated matrix engine ~3–8× AVX-512 for INT8/BF16 GEMM,
and it powers KTransformers' headline numbers (DeepSeek on dual Sapphire + 4090:
**prefill 185–286 t/s, decode ~13–16 t/s**). AMD has no equivalent. So why EPYC?

Because the two phases bottleneck differently and AMX only helps one:
- **Prefill (compute-bound): AMX wins.** If long-prompt / big-context ingestion
  is your pain, Intel + KTransformers is genuinely faster here.
- **Decode (bandwidth-bound): AMX adds nothing** — decode = bandwidth ÷
  active-bytes/token, and the lever is memory *channels*:

| Platform | channels | ~bandwidth | AMX | decode vs EPYC |
|---|---|---|---|---|
| Xeon Sapphire Rapids | 8 × DDR5-4800 | ~307 GB/s | ✅ | slower |
| Xeon Emerald Rapids | 8 × DDR5-5600 | ~358 GB/s | ✅ | slower |
| EPYC Genoa | 12 × DDR5-4800 | ~460 GB/s | ❌ | — |
| EPYC Turin | 12 × DDR5-6000 | ~576 GB/s | ❌ | — |
| Xeon Granite Rapids-AP | 12 × DDR5 / MRDIMM-8800 | ~615–840 GB/s | ✅ | faster (both) |

So the affordable AMX boxes (Sapphire/Emerald Rapids) are **only 8-channel** →
they *decode slower* than a 12-channel EPYC despite AMX; KTransformers' ~13–16
t/s decode ≈ EPYC Turin + `ik_llama.cpp` (~15–22). The one Intel platform that
truly beats EPYC on both is **Granite Rapids-AP (12-channel + MRDIMM-8800)** —
also the **most expensive** option (top-bin Xeon + scarce, pricey MRDIMMs), so
it loses on bang-for-buck. And AMX's payoff is locked to **KTransformers**, which
is Intel-centric (no real AMD support) and more complex than `ik_llama.cpp`.

**Verdict:** for value-driven hosting where interactive *decode* matters,
12-channel EPYC ≥ 8-channel Sapphire/Emerald Rapids, and you recover much of
AMX's prefill edge by offloading attention to the GPU anyway. Go Intel only if
huge-prompt prefill is your top priority (Granite Rapids-AP, loose budget) or you
specifically want to run KTransformers.

## The two builds (DeepSeek-671B target, 576 GB)

Both are **single-socket SP5** (Genoa and Turin share the socket), all 12 DDR5
channels filled with **12×48 GB = 576 GB**, NVIDIA GPU, `ik_llama.cpp`. They
differ mainly in CPU generation (→ bandwidth → decode speed) and GPU class.

> **Core-count note:** decode is bandwidth-bound, so **32–64 cores is plenty** —
> don't overpay for 96c. Extra cores only help prefill/concurrency. The "P"
> SKUs (single-socket-only, e.g. 9354P/9554P) are cheaper and fine here.

### ⭐ Value build — EPYC Genoa (Zen4)

| Part | Pick | ~Price |
|---|---|---|
| CPU | EPYC **9354P (32c)** or 9554P (64c), single-socket | $2.0–3.5k |
| Board | Supermicro **H13SSL-N** (SP5; takes Genoa *and* Turin) | $0.9–1.1k |
| RAM | **12×48 GB DDR5-5600/6000 RDIMM = 576 GB** (runs at 4800 on Genoa) | $5–7k |
| GPU | **RTX A6000 48 GB** (used, Ampere) or Quadro RTX 8000 48 GB | $2.0–3.8k |
| PSU/chassis/NVMe/cooling | 1.6 kW PSU, 4U or workstation, 2 TB NVMe | $0.8–1.5k |
| **Total** | | **~$11–16k** |

- ~440 GB/s, AVX-512, PCIe 5.0. **DeepSeek-671B Q4 ≈ ~10–15 t/s hybrid** (in
  your "fine," edging into "ideal"). Prefill much faster (GPU does attention).

### No-compromise build — EPYC Turin (Zen5)

| Part | Pick | ~Price |
|---|---|---|
| CPU | EPYC **9555 (64c)** or 9575F (high-freq) | $4–8k |
| Board | Supermicro H13SSL-N (same SP5 board) | $0.9–1.2k |
| RAM | **12×48 GB DDR5-6000 RDIMM = 576 GB** (full ~570 GB/s) | $6–9k |
| GPU | **RTX 6000 Ada 48 GB** (FP8) or **RTX PRO 6000 Blackwell 96 GB** | $5.6–8.5k |
| PSU/chassis/NVMe/cooling | 1.6 kW PSU, 4U, 2 TB NVMe | $0.8–1.5k |
| **Total** | | **~$18–28k** |

- ~570 GB/s + native full-width AVX-512. **DeepSeek-671B Q4 ≈ ~15–22 t/s
  hybrid** — hits your interactive ideal. A 96 GB Blackwell card pulls more
  experts off the RAM path for a further bump.

### The hedge (best of both): buy the upgrade path
**Buy the H13SSL-N board + 12×48 GB DDR5-6000 RDIMMs now, start with a cheaper
Genoa CPU, drop in a Turin CPU later.** DDR5-6000 sticks run at 4800 on Genoa
today and unlock their full 6000 speed on Turin — so the bandwidth upgrade
costs *only the CPU swap*, no RAM re-buy. Lets you start at Value cost and reach
No-compromise bandwidth when budget/need says so. (Pair with a 48 GB GPU now;
add/upgrade GPU independently.)

### Budget alternative — DDR4 Milan (only if you must)
EPYC 7713 + used SP3 board + 512 GB used DDR4 + RTX 3090 ≈ **$5–7k**, but
**no AVX-512** (AVX2 only) and ~190 GB/s → **~6–8 t/s** hybrid, and a dead-end
socket. Cheapest way to run >256 GB *at all*, but it caps you below your ideal
and has no upgrade path. Fully specced below; not recommended given your speed
goal.

---

## Why not just use hekaton

hekaton can *hold* these models (1.5 TB RAM) but its Xeon E7-4850 v2 is
**AVX1-only (no AVX2/FMA/AMX)**, which (a) blocks `ik_llama.cpp` and
KTransformers entirely, (b) has ~190 GB/s DDR3 across 4 NUMA, and (c) only
PCIe 3.0 for a GPU. So even with a GPU it's stuck on plain `llama.cpp` at low
single-digit decode. It's a fine novelty/holding box; it is not the path to fast
giant-model inference. (Full reasoning in the hekaton speed-up analysis.)

---

## Open decisions (remaining)

Scope is set (DeepSeek-671B target, 576 GB, 15+ t/s ideal). What's left:

1. **Value now vs. the hedge vs. full No-compromise.** Strong recommendation:
   **the hedge** — board + DDR5-6000 RAM + 48 GB GPU + a Genoa CPU now, Turin
   later. You start ~$11–16k and reach ~570 GB/s with just a CPU swap.
2. **GPU: 48 GB now, or stretch to 96 GB Blackwell?** 48 GB (A6000/RTX 6000 Ada)
   is the value pick and enough for 671B hybrid; 96 GB meaningfully lifts decode
   by holding more experts. Can be upgraded independently of CPU/RAM.
3. **CPU SKU / core count.** 32c (9354P) is enough for decode; 64c (9554P/9555)
   if you want faster prefill or concurrency. Don't pay for 96c.
4. **Chassis/cooling** — confirm 48 GB passive cards (A6000/RTX 6000 Ada) vs a
   chassis with airflow; consumer 3090/4090 axial coolers are a poor server fit.

Once you pick, next step is a finalized parts list with specific SKUs + a quick
`ik_llama.cpp` launch-flag cheat-sheet for DeepSeek-671B on this box.

## Finalized parts list — the hedge (buy now, Turin later)

**Strategy:** buy the SP5 board + 576 GB of **DDR5-6000** RDIMMs + a 48 GB GPU
now, paired with a **cheap, temporary Genoa CPU**. The 6000-speed RAM runs at
4800 on Genoa today and unlocks its full ~570 GB/s the day you drop in a Turin
CPU — so the bandwidth jump costs only a CPU swap, no RAM re-buy. Because the
Genoa chip is a placeholder you'll replace, **buy the cheapest adequate one
(used/QS is fine)** and recover most of its cost at resale.

### Buy now (~$13–17k)

| # | Part | Specific pick | Qty | ~Price | Notes |
|---|---|---|---|---|---|
| CPU (temp) | EPYC Genoa, single-socket | **EPYC 9354P** (32c) or used/QS **9334** (32c) | 1 | $1.5–3.0k | placeholder; 32c is plenty for decode. Buy cheap — you'll swap it. |
| Board | Supermicro **H13SSL-N** | **rev 2.x, latest BIOS** (must support 9005/Turin) | 1 | $0.9–1.1k | 12 DIMM slots = all 12 channels. Confirm Turin on QVL before buying. |
| RAM | **48 GB DDR5-6000 ECC RDIMM** (1DPC), board-QVL module | 12 | $6.5–9.0k | **fill all 12 channels.** 6000-rated so Turin later runs full speed; runs at 4800 on Genoa now. The cost driver. |
| GPU | **NVIDIA RTX A6000 48 GB** (Ampere, passive blower, 300 W) | 1 | $3.0–3.8k | value 48 GB, server-friendly cooling. Alt: RTX 6000 Ada 48 GB (+FP8, ~$5.6k). |
| PSU | 1.6 kW 80+ Platinum/Titanium | redundant or single | 1 | $0.3–0.5k | sized for a future 600 W 96 GB Blackwell GPU. |
| Chassis | 4U rackmount (e.g. Supermicro CSE-846) **or** deep E-ATX/SSI-EEB tower w/ strong front-to-back airflow | 1 | $0.3–0.6k | passive A6000 needs chassis airflow. |
| CPU cooler | **Arctic Freezer 4U-SP5** (air) or SP5 AIO | 1 | $0.06–0.15k | SP5 socket-specific. |
| Storage | 2 TB (or 4 TB) Gen4 NVMe | 1–2 | $0.2–0.4k | DeepSeek-671B Q4 ≈ 380 GB; size for several models. |

> **Sanity checks before ordering:** (1) RAM must be on the H13SSL-N QVL at
> DDR5-6000 *and* validated for both 9004/9005; (2) confirm board rev/BIOS lists
> EPYC 9005 (Turin) support; (3) 1DPC only — single-socket SP5 is 12 slots, one
> DIMM per channel, so 12×48 GB is the max-bandwidth config.

### Upgrade later (net ~$3–6k) → No-compromise

| Swap | To | ~Price | Effect |
|---|---|---|---|
| CPU | **EPYC 9555 (64c)** value, or **9575F (64c, ~5 GHz)** for best prefill | $4–8k (− resale of the Genoa, ~$1.5–2.5k) | RAM jumps 4800→6000 (~440→~570 GB/s) → DeepSeek-671B **~10–15 → ~15–22 t/s** |
| GPU (optional) | add/replace with **RTX PRO 6000 Blackwell 96 GB** | ~$8.5k | more experts resident → further decode bump |

Board + RAM + PSU + chassis all carry over. No RAM re-buy — that's the whole point.

### `ik_llama.cpp` launch cheat-sheet (DeepSeek-671B on this box)

**Build (AVX-512 + CUDA):**
```bash
cmake -B build -DGGML_CUDA=ON -DGGML_AVX512=ON -DGGML_AVX512_VNNI=ON \
      -DGGML_AVX512_BF16=ON -DGGML_SCHED_MAX_COPIES=1
cmake --build build -j --config Release
```

**BIOS:** memory at rated speed (XMP/EXPO not used on RDIMM — set DDR5-6000/4800
manually if needed); **NUMA Nodes Per Socket = NPS1**; SMT on; determinism =
power/performance.

**Run (single socket, experts in RAM, everything else on the 48 GB GPU):**
```bash
numactl -N 0 -m 0 \
  ./build/bin/llama-server \
    -m /models/DeepSeek-R1-Q4_K_M.gguf \
    -ot exps=CPU \            # routed experts stay in RAM
    -ngl 99 \                 # all non-expert layers (attn/dense/shared) -> GPU
    -fmoe \                   # fused MoE dispatch
    -mla 2 -fa \              # DeepSeek MLA + flash attn (KV fits 48 GB)
    -c 32768 -t 32 \          # context; threads = physical cores on the node
    -b 2048 -ub 512 \         # prefill batch tuning
    --host 0.0.0.0 --port 5391 --alias deepseek-r1-local
```

Tuning notes: bump `-c` for longer context (MLA keeps KV small); add `-ser 7,1`
to trade a little quality for ~10–30% more decode if you want; **do not** use
`-rtr` in hybrid CPU+GPU mode (mis-places tensors). Verify the experts-on-CPU
regex against the model's tensor names (`-ot "\.ffn_.*_exps\.=CPU"` is the
explicit form). Confirm GGUF + arch support for the exact DeepSeek build before
buying weights; M3/Nemotron-Ultra arch support is not there yet (see table).

## Finalized parts list — budget (DDR4 Milan, just for comparison)

Single-socket EPYC Milan. High core count to partly offset the lack of AVX-512.
Fits DeepSeek-671B at Q4 (512 GB), but it's a **dead-end socket**, ~half the
bandwidth of Genoa, and below the 15+ t/s ideal — included for reference.

### Buy (~$5–7k, no later upgrade)

| # | Part | Specific pick | Qty | ~Price | Notes |
|---|---|---|---|---|---|
| CPU | EPYC **7713** (64c Milan) or **7702** (64c Rome) | 1 | $0.5–0.9k | 64c helps offset AVX2-only kernels. Cheap used. |
| Board | Supermicro **H12SSL-i** (SP3, 8 DIMM, PCIe 4) | 1 | $0.4–0.7k | single-socket, 8 channels. **Dead-end socket** — no Zen4/5. |
| RAM | **64 GB DDR4-3200 ECC RDIMM** | 8 | $2.0–3.2k | 512 GB, fills all 8 channels. Used DDR4 ≈ $4–6/GB. |
| GPU | **RTX 3090 24 GB** (used) | 1 | $0.7–1.0k | prefill win, few experts resident. Axial cooler → needs airflow. |
| PSU | 1.0–1.2 kW 80+ Gold | 1 | $0.2–0.3k | 3090 350 W + EPYC ~225 W. |
| Chassis | 4U or tower w/ front-to-back airflow | 1 | $0.3–0.5k | |
| CPU cooler | SP3 cooler (Arctic Freezer 4U-SP3 / Noctua TR4-SP3) | 1 | $0.06–0.1k | |
| Storage | 2 TB Gen4 NVMe | 1 | $0.15–0.3k | |
| **Total** | | | | **~$5–7k** |

**Performance / caveats:**
- ~190 GB/s (8-ch DDR4-3200). **No AVX-512** → `ik_llama.cpp`/`llama.cpp` run on
  AVX2 (Milan has AVX2+FMA) but **without the AVX-512 IQK fast kernels**, so
  decode lands at plain-`llama.cpp` tier. DeepSeek-671B Q4 ≈ **~3–4 t/s CPU-only,
  ~6–8 t/s with the 3090** (GPU does attention/dense; experts stream from DDR4).
- **No upgrade path** (SP3 won't take Genoa/Turin) — the opposite of the hedge.
- KTransformers technically meets its AVX2+FMA minimum here, but with no
  AVX-512/AMX it gains nothing → still use `ik_llama.cpp`/`llama.cpp`.
- **Build difference:** compile **without** `-DGGML_AVX512=ON` (AVX2 default);
  launch flags otherwise same as the hedge cheat-sheet above.

**vs. the hedge:** saves ~$8–10k up front but gives up ~2× decode bandwidth,
the AVX-512 fast kernels, and any upgrade path. The hedge's extra cost is mostly
DDR5 RAM — which, at 2026 prices, is exactly what you're paying a premium for.

## Sources

Platform/RAM/benchmarks: llama.cpp Discussions #11765 / #11733 / #11881;
ik_llama.cpp Discussions #258 / #477; ahelpme.com EPYC 9554 bench; Digital
Spaceport EPYC DeepSeek; Chips and Cheese / StorageReview Turin; Phoronix DDR5
EPYC scaling; TrendForce / Tom's Hardware / TechPowerUp DRAM pricing; RunAIHome
Kimi K2 guide; Kimi K2.7-Code release 2026-06-12 (MarkTechPost; HF
`unsloth/Kimi-K2.7-Code-GGUF` for exact quant sizes). GPU pricing/fit: XiongjieDai GPU benchmarks, LocalLLM.in 96 GB
RTX PRO 6000, eBay used-market spot checks. (Full URL list in the research
threads that produced this doc.)
