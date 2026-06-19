# Verification checklist

This code was written without a local Flutter toolchain, so verify it compiles
against the exact package versions you resolve. Work top‑down:

## 1. Resolve dependencies
```bash
cd flutter_speaker_llm
flutter pub get
cd example && flutter create . --platforms=android,ios,macos,windows && flutter pub get
```
If a version constraint fails, run `flutter pub upgrade --major-versions` and
note the resolved versions of `sherpa_onnx`, `flutter_gemma`,
`flutter_gemma_litertlm`, `flutter_tts`, `record`.

## 2. Static analysis
```bash
flutter analyze
```
Most likely things to reconcile (API drift between versions):
- **sherpa_onnx config constructors** — field names / required vs optional on
  `OnlineModelConfig`, `KeywordSpotterConfig`, `VadModelConfig`,
  `OfflineWhisperModelConfig`, `OfflineRecognizerConfig`. Check
  `package:sherpa_onnx/sherpa_onnx.dart`.
- **`sherpa_onnx.initBindings()`** — confirm the exact init call name.
- **flutter_gemma** — confirm the model‑install API
  (`installModelFromNetworkWithProgress` vs the `installModel(...).fromNetwork`
  builder), the `Tool`/function‑call response types, and `ModelType` value used
  for Qwen.
- **`record`** — confirm `RecordConfig`/`AudioEncoder.pcm16bits` and
  `startStream` signatures.

## 3. Models
- Put real download URLs in `ModelSources` (see `docs/MODELS.md`) or mirror the
  files to your CDN in the documented layout.
- First launch downloads them; check progress callbacks fire and files land in
  the app documents dir.

## 4. Runtime smoke test
- Grant microphone permission.
- Say "hi synlink", then a command ("请帮我对极轴" / "goto M31").
- Confirm: wake detected → transcript appears → command + params dispatched.
- Say gibberish → confirm spoken "didn't catch that" in your language.

## 5. Per‑platform
- **Android:** `RECORD_AUDIO` granted; background pause works.
- **iOS:** mic usage string present; TTS + mic coexist (no audio session crash).
- **macOS:** audio‑input + network‑client entitlements in *both* Debug and
  Release entitlements files.
- **Windows/macOS:** `flutter_gemma_litertlm` present and `.litertlm` LLM loads.
