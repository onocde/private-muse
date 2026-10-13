# Private Muse

A single-file Windows app that gives you a private, offline AI chat on your own PC.
No account, no internet after setup, no telemetry — the model lives on your machine and answers only to you.

## What the .exe does

1. **Double-click `PrivateMuse.exe`.** A small black window opens (keep it open while you chat — closing it quits the app).
2. **First launch only:** it unpacks its built-in llama.cpp engine, then downloads the AI model
   from Hugging Face with a progress bar (~4.7 GB — do this on Wi-Fi).
3. A chat window opens in app mode (Edge/Chrome). After the first download, everything runs
   **100% offline** — airplane mode fine.

## The model

- **Default:** Qwen2.5-Coder **7B** Abliterated, Q4_K_M (bartowski's GGUF of huihui-ai's abliterated fine-tune).
  Abliterated = the refusal training was removed by the model maker; we add no filters of our own.
  Chosen for the Acer Nitro 5 (16 GB RAM): it fits almost entirely on the GPU via Vulkan,
  so expect roughly **25–45 tokens/sec**. On the CPU fallback expect ~6–10 tok/s.
- **Optional in Settings:** Qwen2.5-Coder **14B** Abliterated Q4_K_M — smarter, but only partly
  GPU-offloaded on this laptop (~8–15 tok/s) and it cannot use the 128k context within 16 GB RAM.
  It downloads (~9 GB) only if you pick it.

## Context length

Settings → Context: 8k / 16k / **32k (default)** / 64k / **128k max**.
Larger context = the AI remembers more of the conversation, but uses more RAM
(the 7B at 128k needs roughly 4 GB extra) and replies get slower. The model family supports 128k natively.

## Where files live

Everything goes to `%LOCALAPPDATA%\PrivateMuse\`:

- `models\` — the downloaded .gguf model(s) (plus a `.part` file while downloading; interrupted downloads resume)
- `engine\` — the unpacked llama.cpp builds (Vulkan GPU build + CPU fallback build)
- `settings.json` — your model/context choices
- `engine.log` — engine output, useful if anything ever fails to start

To uninstall: delete the .exe and that folder. Nothing is installed system-wide.

## Tech notes

- Engine: llama.cpp **b11577** `llama-server` (official release binaries), embedded in the .exe
  and unpacked on first run. The app talks to it over a local-only (127.0.0.1) API — nothing is exposed to your network.
- Launcher/UI: Go, standard library only. The UI is served locally and opened in Edge/Chrome `--app` mode.
- GPU path: Vulkan build of llama.cpp (works with NVIDIA drivers out of the box).
  If the GPU engine fails to start for any reason, the app automatically falls back to the CPU build.
