# Models & download URLs

The app ships small; these files download on first launch. Two ways to supply
them:

1. **Your CDN (recommended):** mirror the files in the layout below and call
   `ModelSources.fromBaseUrl('https://cdn.example.com/synlink-models')`.
2. **Official upstream:** `ModelSources.defaults()` (URLs below). These may be
   slow/blocked in some regions — mirroring to your CDN avoids that.

> ⚠️ The exact upstream URLs/filenames below should be confirmed against the
> current `sherpa_onnx` and `flutter_gemma` releases before shipping. The
> `fromBaseUrl` layout is stable and recommended.

## CDN layout

```
<base>/
  kws/      encoder.onnx  decoder.onnx  joiner.onnx  tokens.txt  keywords.txt
  vad/      silero_vad.onnx
  whisper/  tiny-encoder.int8.onnx  tiny-decoder.int8.onnx  tiny-tokens.txt
            base-encoder.int8.onnx  base-decoder.int8.onnx  base-tokens.txt
  llm/      qwen2.5-0.5b-instruct.task        (Android / iOS)
            qwen2.5-0.5b-instruct.litertlm    (Windows / macOS)
  tts/      model.onnx  tokens.txt  lexicon.txt   (optional offline TTS)
```

## 1. Wake word — keyword spotter (KWS)

English streaming zipformer keyword spotter. Approx. 3–15 MB.

- Upstream: `sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01`
  (HuggingFace: `pkufool/...`).
- Files: encoder / decoder / joiner `.onnx`, `tokens.txt`, `keywords.txt`.

### Generating `keywords.txt` for "hi synlink"

`keywords.txt` holds the phrase as BPE tokens, optionally with a score. Generate
it with sherpa-onnx's `text2token`:

```bash
# from a sherpa-onnx checkout
python3 -m sherpa_onnx.text2token \
  --tokens tokens.txt --tokens-type bpe --bpe-model bpe.model \
  --text "HI SYNLINK" --output keywords.txt
# tune sensitivity by appending ":1.5" (boost) or "@0.25" (threshold) per line
```

Then host the resulting `keywords.txt` at `<base>/kws/keywords.txt`.

## 2. VAD — Silero

`silero_vad.onnx` (~2 MB).
Upstream: `https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx`

## 3. ASR — Whisper (multilingual, int8)

- **tiny** (~40–75 MB): `csukuangfj/sherpa-onnx-whisper-tiny`
  → `tiny-encoder.int8.onnx`, `tiny-decoder.int8.onnx`, `tiny-tokens.txt`
- **base** (~80–140 MB): `csukuangfj/sherpa-onnx-whisper-base`
  → `base-encoder.int8.onnx`, `base-decoder.int8.onnx`, `base-tokens.txt`

Both are multilingual (zh/en/ja/es/…). Pick via
`SynlinkConfig.whisperModel`.

## 4. LLM — Qwen2.5-0.5B-Instruct (function calling)

Fetched by `flutter_gemma`. Two formats:

- **`.task`** for Android/iOS/Web (MediaPipe).
- **`.litertlm`** for Windows/macOS (LiteRT-LM; `flutter_gemma_litertlm`).

Look for the Qwen2.5-0.5B-Instruct assets in the LiteRT community / flutter_gemma
model list and set `llmTaskUrl` / `llmLitertlmUrl` (or host on your CDN). Approx.
0.5–0.7 GB.

## 5. Optional offline TTS

Only needed if `voiceFeedback.engine == TtsEngineType.sherpaOffline`. Default is
the OS speech engine (no download). A good small zh+en model:
`vits-melo-tts-zh_en` —
`https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-melo-tts-zh_en.tar.bz2`
(extract `model.onnx`, `tokens.txt`, `lexicon.txt`, `dict/`).
