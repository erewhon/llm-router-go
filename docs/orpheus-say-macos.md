# Local Orpheus TTS on macOS (for `orpheus-say`)

`orpheus-say` speaks text via an Orpheus `/v1/audio/speech` endpoint. On an
Apple-Silicon Mac you can run that endpoint **locally** with
[mlx-audio](https://github.com/Blaizzy/mlx-audio), so synthesis stays on the
laptop — nothing leaves the machine at synth time (only the one-time model
download hits Hugging Face).

> **Apple Silicon only** — mlx-audio uses Apple's MLX framework; Intel Macs
> won't work.
>
> `orpheus-say` talks to this server **directly**, not through the llm-router:
> the router proxies chat/embeddings/rerank only, not `/v1/audio/speech`. On a
> laptop the router (chat: Bedrock / LM Studio) and this Orpheus server run side
> by side on different ports.

## 1. Install

```sh
mkdir -p ~/orpheus && cd ~/orpheus
uv venv --python 3.12
source .venv/bin/activate

# mlx-audio + server extras, THEN pin mlx/mlx-lm (see the gotcha below)
uv pip install -U 'mlx-audio[all]' 'setuptools<81'
uv pip install 'mlx==0.31.1' 'mlx-lm==0.31.1'
```

### ⚠️ Pin `mlx==0.31.1` — don't skip this

mlx 0.31.2 made GPU streams thread-local, which crashes the **mlx-audio server**
on its very first request:

```
RuntimeError: There is no Stream(gpu, 0) in current thread
```

`mlx==0.31.1` + `mlx-lm==0.31.1` (a matched pair) is the only pre-bug combo
mlx-audio still accepts (fallback: a matched `0.31.0` pair). Install
`mlx-audio[all]` first, then force the pin **last** so it isn't upgraded back.
Never mix versions (e.g. mlx 0.31.1 + mlx-lm 0.31.3 → API mismatch). The CLI
(`python -m mlx_audio.tts.generate`) is unaffected; only the *server* path
breaks, because it synthesizes on a worker thread.

## 2. Run the server

```sh
mlx_audio.server --host 127.0.0.1 --port 8000
```

The Orpheus model (`mlx-community/orpheus-3b-0.1-ft-4bit`, ~2–3 GB) downloads
from Hugging Face on the **first** request and is cached in
`~/.cache/huggingface` afterwards, so the first `orpheus-say` call is slow while
it fetches + loads; later calls are fast.

## 3. Point `orpheus-say` at it

```sh
export ORPHEUS_URL=http://127.0.0.1:8000
export ORPHEUS_MODEL=mlx-community/orpheus-3b-0.1-ft-4bit   # server keys off the HF repo id

orpheus-say "Hello from a local Orpheus on my Mac."
```

Add those two exports to `~/.zshrc` to make them permanent. Voices (`--voice`,
default `tara`) and the emotion tags — `<laugh>`, `<chuckle>`, `<sigh>`,
`<gasp>`, `<groan>`, `<yawn>`, `<cough>`, `<sniffle>` — work exactly as on the
fleet; it's the same base model.

## 4. (Optional) Auto-start at login — launchd LaunchAgent

Run the server always-on in the background (no terminal to babysit):

```sh
mkdir -p ~/Library/LaunchAgents ~/Library/Logs

cat > ~/Library/LaunchAgents/org.byrnes.orpheus.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>             <string>org.byrnes.orpheus</string>
    <key>ProgramArguments</key>
    <array>
        <string>${HOME}/orpheus/.venv/bin/mlx_audio.server</string>
        <string>--host</string>  <string>127.0.0.1</string>
        <string>--port</string>  <string>8000</string>
    </array>
    <key>RunAtLoad</key>         <true/>
    <key>KeepAlive</key>         <true/>
    <key>ProcessType</key>       <string>Background</string>
    <key>WorkingDirectory</key>  <string>${HOME}/orpheus</string>
    <key>StandardOutPath</key>   <string>${HOME}/Library/Logs/orpheus-mlx.log</string>
    <key>StandardErrorPath</key> <string>${HOME}/Library/Logs/orpheus-mlx.log</string>
</dict>
</plist>
EOF

# load + start it (modern launchctl)
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/org.byrnes.orpheus.plist
launchctl kickstart -k gui/$(id -u)/org.byrnes.orpheus
```

Manage it:

```sh
launchctl print gui/$(id -u)/org.byrnes.orpheus | head   # status
tail -f ~/Library/Logs/orpheus-mlx.log                   # logs
launchctl kickstart -k gui/$(id -u)/org.byrnes.orpheus   # restart
launchctl bootout gui/$(id -u)/org.byrnes.orpheus        # stop + unload
```

After editing the plist, `bootout` then `bootstrap` again to reload it. (Older
macOS without `bootstrap`: `launchctl load -w ~/Library/LaunchAgents/org.byrnes.orpheus.plist`.)

## Notes & troubleshooting

- **Serial only.** Concurrent requests crash the mlx-audio server (a separate
  upstream bug). `orpheus-say` synthesizes one chunk at a time, so it's safe —
  just don't run two `orpheus-say`s at once.
- **Latency.** The server buffers the whole WAV before returning, so first-audio
  ≈ full synth time (~4–7 s/clip). `orpheus-say`'s sentence chunking hides this
  by starting playback after the first sentence. 4-bit runs ~0.93× realtime
  (speech-only) on an M1 Max; newer chips are faster.
- **Still crashes on the first request** → confirm the pin actually stuck:
  `uv pip show mlx mlx-lm | grep -i version` (both must be `0.31.1`).
- **Corporate network** → the one-time HF download respects `HTTPS_PROXY` /
  `HF_ENDPOINT`.
- Prefer the 6-bit model? Set `ORPHEUS_MODEL=mlx-community/orpheus-3b-0.1-ft-6bit`
  — higher quality but slower (~0.76× realtime; may under-run on synthesis).
