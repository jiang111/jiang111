# flutter_speaker_llm

A reusable Flutter library that turns speech into device commands, fully
on‑device:

**wake word → offline speech‑to‑text → on‑device LLM intent recognition →
your command handlers**, with **spoken voice feedback** when a command isn't
understood.

Built for telescope / astrophotography control (polar align, GOTO, focus,
capture, …) but the command set is configurable. Works in **Chinese, English,
Japanese, Spanish and ~100 other languages** because speech recognition uses
multilingual Whisper and the LLM is multilingual.

- **Speech recognition:** [`sherpa_onnx`](https://pub.dev/packages/sherpa_onnx)
  (keyword spotting for the wake word, Silero VAD for endpointing, Whisper for
  transcription) — 100% offline, runs everywhere including Mainland China.
- **Intent recognition:** [`flutter_gemma`](https://pub.dev/packages/flutter_gemma)
  running **Qwen2.5‑0.5B‑Instruct** with function calling.
- **Voice feedback:** [`flutter_tts`](https://pub.dev/packages/flutter_tts)
  (OS speech engine, no extra download) — optional fully‑offline sherpa TTS.

> The app ships small. Large model files (LLM + ASR) are **downloaded on first
> launch** from URLs you control (point them at your own CDN).

Supported platforms: **Android, iOS, macOS, Windows**.

---

## Pipeline

```
                 ┌──────────────── single microphone owner ───────────────┐
mic (16k mono) ─▶│ Silero VAD gate ─▶ KWS "hi synlink" ─▶ VAD endpointing  │
                 └─────────┬───────────────────┬─────────────────┬─────────┘
                           │ (silence: cheap)   │ (speech only)    │ utterance
                           ▼                    ▼                  ▼
                      idle / listen        run keyword         Whisper ASR ──▶ text
                                            spotter                              │
                                                                                ▼
                                              Qwen2.5‑0.5B (flutter_gemma, function calling)
                                                                                │
                                              ┌──────────────── matched? ───────┘
                                              ▼ yes                    ▼ no / error
                                     your CommandHandler         speak "didn't catch that"
                                     (e.g. mount.goto("M31"))    in the user's language
```

State machine:
`idle → listeningWake →(hi synlink)→ capturing(VAD) →(silence)→ transcribing →
reasoning(LLM) → dispatch | speaking(TTS) → listeningWake`.

---

## Built‑in commands

| Command (中文) | Tool name        | Params           |
|----------------|------------------|------------------|
| 对极轴         | `polar_align`    | –                |
| GOTO 到目标    | `goto_target`    | `target` (e.g. `M31`) |
| 对焦           | `focus`          | –                |
| 拍摄           | `capture`        | –                |
| 拍单张         | `single_shot`    | –                |
| 下载图片       | `download_image` | –                |

The library only **recognises** commands and extracts parameters. **You supply
the behaviour** by registering handlers. You can also add your own tools.

---

## Quick start

```dart
import 'package:flutter_speaker_llm/flutter_speaker_llm.dart';

final engine = SynlinkEngine(
  config: SynlinkConfig(
    wakeWord: 'hi synlink',
    whisperModel: WhisperModelSize.base,        // tiny = smaller/faster, base = more accurate
    powerMode: PowerMode.balanced,
    pauseInBackground: true,
    keepModelsWarmIdle: const Duration(minutes: 3),
    // Point these at your own CDN (see "Models & download"):
    modelSources: ModelSources.fromBaseUrl('https://cdn.example.com/synlink-models'),
    voiceFeedback: const VoiceFeedbackConfig(enabled: true), // system TTS
  ),
);

// 1) Download / verify models (first launch only).
await engine.ensureModelsReady(onProgress: (p) {
  print('Downloading ${p.stage}: ${(p.fraction * 100).toStringAsFixed(0)}%');
});

// 2) Wire up what each command does.
engine.registry.on(CommandType.polarAlign,  (c) => mount.startPolarAlign());
engine.registry.on(CommandType.gotoTarget,  (c) => mount.goto(c.target!));   // c.target == "M31"
engine.registry.on(CommandType.focus,       (c) => camera.autoFocus());
engine.registry.on(CommandType.capture,     (c) => camera.startCapture());
engine.registry.on(CommandType.singleShot,  (c) => camera.singleShot());
engine.registry.on(CommandType.downloadImage,(c) => gallery.downloadLatest());

// 3) Start always‑on listening.
await engine.start();

// Optional: observe everything.
engine.stateStream.listen((s) => print('state: $s'));
engine.transcriptStream.listen((t) => print('heard: $t'));
engine.commandStream.listen((c) => print('command: $c'));

// Later:
await engine.stop();
await engine.dispose();
```

### Adding a custom command

```dart
engine.addTool(SynlinkTool(
  name: 'set_exposure',
  description: 'Set the camera exposure time in seconds',
  parameters: {
    'type': 'object',
    'properties': {'seconds': {'type': 'number'}},
    'required': ['seconds'],
  },
));
engine.registry.onTool('set_exposure', (c) => camera.setExposure(c.params['seconds']));
```

---

## Models & download

Files are downloaded to the app's documents directory on first launch. The LLM
is fetched by `flutter_gemma`; the sherpa assets by the bundled
`DownloadManager`. **All URLs are configurable** — host the files on your own
CDN and use `ModelSources.fromBaseUrl(...)`, which expects this layout:

```
<base>/
  kws/      encoder.onnx  decoder.onnx  joiner.onnx  tokens.txt  keywords.txt
  vad/      silero_vad.onnx
  whisper/  tiny-encoder.int8.onnx  tiny-decoder.int8.onnx  tiny-tokens.txt
            base-encoder.int8.onnx  base-decoder.int8.onnx  base-tokens.txt
  llm/      qwen2.5-0.5b-instruct.task        (Android / iOS)
            qwen2.5-0.5b-instruct.litertlm    (Windows / macOS)
```

> Default (official) download URLs are listed in
> [`docs/MODELS.md`](docs/MODELS.md). Use `ModelSources.defaults()` to pull from
> them directly, or mirror the files to your CDN for reliable downloads in all
> regions.

### Wake word

The wake word "hi synlink" is configured in `kws/keywords.txt`. To use a
different phrase, regenerate that file with sherpa‑onnx's
`text2token` tool — see [`docs/MODELS.md`](docs/MODELS.md).

---

## Platform setup

This package contributes no permissions automatically — add them to the **host
app**:

**Android** (`android/app/src/main/AndroidManifest.xml`):
```xml
<uses-permission android:name="android.permission.RECORD_AUDIO"/>
<uses-permission android:name="android.permission.INTERNET"/>
```

**iOS** (`ios/Runner/Info.plist`):
```xml
<key>NSMicrophoneUsageDescription</key>
<string>Voice commands need the microphone.</string>
```

**macOS** (`macos/Runner/*.entitlements` — both Debug and Release):
```xml
<key>com.apple.security.device.audio-input</key><true/>
<key>com.apple.security.network.client</key><true/>
```

**Windows:** no manifest change required.

**Desktop LLM:** on Windows/macOS `flutter_gemma` requires the
`flutter_gemma_litertlm` package and `.litertlm` models. Add it to the **host
app's** `pubspec.yaml` (kept out of this library's deps so mobile builds stay
lean):
```yaml
dependencies:
  flutter_gemma_litertlm: ^0.9.0   # desktop only — verify version
```

---

## Performance & power (always‑on mic)

- **Cheap‑first cascade:** a tiny Silero VAD runs continuously; the heavier
  keyword spotter only runs when speech is present; Whisper + the LLM only run
  after the wake word. A pre‑roll ring buffer prevents clipping the wake word.
- **Background isolate:** the synchronous sherpa‑onnx FFI inference runs in a
  dedicated isolate so it never blocks the UI or the audio callbacks.
- **Lazy load + idle unload:** the big Whisper/LLM models load on first wake and
  unload after `keepModelsWarmIdle`, freeing RAM.
- **int8 models, native 16 kHz capture** (no resampling), 100 ms chunking.
- **`PowerMode`** trades sensitivity for battery (`low` / `balanced` /
  `performance`).
- **`pauseInBackground`** stops listening when the app is backgrounded.

## Audio / microphone conflicts

- **One microphone owner** broadcasts the same stream to KWS / VAD / ASR — the
  mic is never opened twice, avoiding "mic busy" errors.
- **Audio session / focus:** iOS uses `playAndRecord` so TTS and recording
  coexist; Android requests audio focus and uses the `VOICE_RECOGNITION` source
  (built‑in echo cancellation). Interruptions (calls, route changes) pause/resume.
- **No self‑trigger:** while speaking feedback, wake detection is suppressed.

---

## Building & running the example

```bash
cd flutter_speaker_llm/example
flutter create . --platforms=android,ios,macos,windows   # generate platform folders
flutter pub get
flutter run -d macos        # or windows / an attached android/ios device
```

> This repository was authored in an environment without a Flutter toolchain,
> so the Dart code is written against the documented package APIs and has **not
> been compiled here**. Run `flutter analyze` first and expect to reconcile
> minor API differences with the exact `sherpa_onnx` / `flutter_gemma` versions
> you resolve. Verification checklist: [`docs/VERIFY.md`](docs/VERIFY.md).

---

## License

MIT — see [LICENSE](LICENSE).
