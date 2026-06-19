# CLAUDE.md — flutter_speaker_llm

给 Claude Code 的项目上下文。开始改代码前请先读本文件，再按需查阅
`docs/REQUIREMENTS.md`（需求）与 `docs/DESIGN.md`（技术设计）。

## 这是什么

一个**可复用 Flutter 库** + **示例 App**，把语音变成 App 指令，全程**本地离线**：

> 唤醒词 → 离线 Whisper 语音转文字 → 本地 LLM 语义/意图识别 → 集成方的指令
> handler；不匹配时用 TTS 语音提示。

- 语音：`sherpa_onnx`（KWS 唤醒、Silero VAD 断点、Whisper 多语种 ASR）
- 语义：`flutter_gemma` 跑 Qwen2.5‑0.5B（结构化 JSON 输出意图+参数+语言）
- 反馈：`flutter_tts`（系统 TTS，默认；可选 sherpa 离线 TTS）
- 目标平台：Android / iOS / macOS / Windows
- 库**不含任何内置指令**，指令由集成方通过 `SynlinkConfig.commands` 配置

## 目录结构

```
lib/flutter_speaker_llm.dart        # 公开 API barrel（只导出门面层）
lib/src/
  engine/      synlink_engine.dart  门面（唯一入口）/ synlink_config.dart 配置
  pipeline/    voice_pipeline.dart  运行时状态机编排
  infer/       speech_engine.dart   推理 isolate 句柄+协议 / speech_worker.dart 后台 isolate
  asr/         wake_word_detector.dart(KWS) / vad.dart(Silero) / whisper_asr.dart
  llm/         intent_service.dart  意图解析 / tools.dart 工具+提示词
               llm_backend.dart 抽象 / gemma_backend.dart flutter_gemma 实现
  tts/         voice_feedback.dart 系统TTS+门面 / sherpa_tts.dart 可选离线TTS
  audio/       audio_manager.dart 单一麦克风 / ring_buffer.dart / pcm.dart
  commands/    command.dart / command_definition.dart / registry.dart
  download/    model_sources.dart URL表 / model_asset.dart / download_manager.dart
  util/        language.dart
example/       消费本库的示例 App（含 6 个望远镜指令的配置示例）
docs/          REQUIREMENTS.md / DESIGN.md / MODELS.md / VERIFY.md
tool/          bootstrap.sh（生成平台壳 + 打权限补丁）
```

## 构建 / 运行

本仓库**只含 Dart 代码**；平台壳（android/ios/macos/windows）由 `flutter create`
生成、未入库。

```bash
cd flutter_speaker_llm
bash tool/bootstrap.sh          # flutter create 四端 + 自动补麦克风/网络权限
cd example && flutter pub get
flutter run -d macos            # 或 windows / 真机
```

> ⚠️ 作者环境没有 Flutter SDK，**代码未经编译**。改动后务必先 `flutter analyze`，
> 并按 `docs/VERIFY.md` 核对。

## 关键约定 / 不变量

- **离线优先**：不依赖任何云端推理；模型首启下载，之后纯本地。
- **模型 URL 全可配**：库不写死地址，`ModelSources.fromBaseUrl('CDN')`；勿提交模型二进制（见 `.gitignore`）。
- **指令按名字派发**：没有 `CommandType` 枚举；`Command.name` + `CommandRegistry.on(name, handler)`。
- **意图用结构化 JSON**：不依赖 flutter_gemma 的原生 function‑call API，靠系统提示让模型输出 `{"command","params","language"}` 再解析（跨版本更稳）。
- **单一麦克风持有者**：`AudioManager` 只开一路麦克风，广播给 KWS/VAD/ASR；切阶段只改路由，绝不反复开关麦克风。
- **推理在后台 isolate**：sherpa FFI 同步阻塞，全部在 `speech_worker`；flutter_gemma 是异步的留主 isolate。
- **懒加载+空闲卸载**：Whisper/LLM 首次唤醒才加载，空闲 `keepModelsWarmIdle` 后卸载。
- **代码注释用英文**，遵循 `flutter_lints`，单引号、`prefer_const`。

## 当前状态与待办（重要）

代码完整但**未编译验证**。最可能要动的：

1. **`gemma_backend.dart`**：flutter_gemma 各版本 API 差异大（`createModel/createChat`、
   响应类型、`installModelFromNetworkWithProgress`、`ModelType`）。所有 flutter_gemma
   调用都集中在这一个文件，便于校准。
2. **sherpa_onnx config 字段名**：`KeywordSpotterConfig/OnlineModelConfig/VadModelConfig/
   OfflineWhisperModelConfig/OfflineRecognizerConfig` 及 `initBindings()`。
3. **模型 URL/文件名**：`model_sources.dart` 的 `defaults()` 与 `docs/MODELS.md`，
   含唤醒词 `keywords.txt` 生成。
4. 平台壳由 `tool/bootstrap.sh` 生成；桌面需宿主 App 加 `flutter_gemma_litertlm`。
5. 尚无单元测试 / CI；`record` 的 `RecordConfig` 字段、`audioplayers` 版本需核对。

详见 `docs/DESIGN.md` 的「已知风险/待办」。

## 扩展点

- 自定义指令：`SynlinkConfig.commands`（`CommandDefinition`）或运行时 `engine.addTool`。
- 换 LLM：实现 `LlmBackend` 注入（替代 `GemmaBackend`）。
- 换 TTS：`VoiceFeedback` 的 `override` 回调，或实现 `TtsEngine`。
