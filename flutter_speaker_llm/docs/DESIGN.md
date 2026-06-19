# 技术设计文档 — flutter_speaker_llm

版本：0.1 ｜ 配套：`REQUIREMENTS.md`（需求）、`MODELS.md`（模型）、`VERIFY.md`（验证）

## 1. 架构总览

分层：**门面 → 编排 → 能力适配（音频/ASR/LLM/TTS/下载）→ 底层库（sherpa_onnx /
flutter_gemma / flutter_tts / record）**。

```
                        ┌───────────────────────── SynlinkEngine（门面）─────────────────────────┐
                        │  config / registry / ensureModelsReady / start / stop / streams        │
                        └───────────────┬───────────────────────────────────────────────────────┘
                                        │ 组装并驱动
                        ┌───────────────▼───────────── VoicePipeline（状态机）──────────────────┐
                        │  AudioManager  SpeechEngine  IntentService  CommandRegistry  VoiceFeedback│
                        └──┬──────────────┬───────────────┬───────────────┬──────────────┬─────────┘
            16k mono PCM   │              │ audio(SendPort)│ JSON intent   │ dispatch     │ TTS
                           ▼              ▼                ▼               ▼              ▼
                     AudioManager   SpeechEngine ── isolate ──► speech_worker:           VoiceFeedback
                     (record, 单一   (句柄/事件流)   KWS门控→唤醒→VAD断点→Whisper        (flutter_tts /
                      麦克风广播)                     events: wake/transcript/empty       sherpa offline)
                                                     ▲
                                                     │ GemmaBackend(LlmBackend) ◄── IntentService
                                                     │   flutter_gemma + Qwen2.5-0.5B（JSON）
                                          DownloadManager(dio) + ModelSources(URL/CDN)
```

## 2. 模块清单

| 模块 | 文件 | 职责 |
|------|------|------|
| 门面 | `engine/synlink_engine.dart` | 组装依赖、下载、生命周期、暴露 API 与流 |
| 配置 | `engine/synlink_config.dart` | `SynlinkConfig` + `PowerMode`/`WhisperModelSize` |
| 编排 | `pipeline/voice_pipeline.dart` | 运行时状态机、抑制、空闲卸载、暂停/恢复 |
| 推理句柄 | `infer/speech_engine.dart` | `SpeechEngine` + `SpeechConfig` + 事件协议 |
| 推理 isolate | `infer/speech_worker.dart` | 后台跑 KWS/VAD/Whisper 的级联状态机 |
| 唤醒 | `asr/wake_word_detector.dart` | sherpa KWS：`feed()`/`poll()` |
| 断点 | `asr/vad.dart` | sherpa Silero VAD 封装 |
| 转写 | `asr/whisper_asr.dart` | sherpa 离线 Whisper |
| 意图 | `llm/intent_service.dart` | 提示词→LLM→解析 JSON→`Command` |
| 工具/提示 | `llm/tools.dart` | `SynlinkTool` + `buildSystemPrompt` |
| LLM 抽象/实现 | `llm/llm_backend.dart` / `llm/gemma_backend.dart` | 后端接口 + flutter_gemma 实现 |
| 反馈 | `tts/voice_feedback.dart` | `VoiceFeedback` + 系统 TTS 引擎 |
| 离线 TTS | `tts/sherpa_tts.dart` | 可选 sherpa OfflineTts |
| 音频 | `audio/audio_manager.dart` | 单一麦克风、广播 Float32 |
| 缓冲/转换 | `audio/ring_buffer.dart` / `audio/pcm.dart` | 预滚动环形缓冲 / PCM16→Float32 |
| 指令 | `commands/{command,command_definition,registry}.dart` | 指令模型/定义/派发 |
| 下载 | `download/{model_sources,model_asset,download_manager}.dart` | URL 表/描述/下载器 |
| 工具 | `util/language.dart` | BCP‑47 映射、脚本启发式、提示文案选择 |

## 3. 关键数据流（时序）

```
mic → AudioManager(Float32 16k) → SpeechEngine.pushAudio → [isolate] speech_worker
  listeningWake: 每帧 feed 给 KWS（保连续）；gateVad 判定说话才 poll 解码
    命中"hi synlink" → 发 wake 事件 → 进入 capturing（用 preroll 预热 captureVad）
  capturing: 喂 captureVad；产出完整语音段 → Whisper 转写 → 发 transcript(text,lang)
[main] VoicePipeline 收到 transcript:
  setSuppressed(true) → IntentService.recognize(text) → GemmaBackend.complete → JSON
  解析为 Command(name,params,language) → registry.dispatch(command)
    命中: 运行 handler（可选成功播报）
    未命中(name 为空): VoiceFeedback.speakNotRecognized(language)
  setSuppressed(false) → 回 listening
```

## 4. 状态机

`PipelineState`：`idle → listening →(wake)→ capturing →(transcript)→ reasoning →
dispatch｜speaking(TTS) → listening`；另有 `paused`（后台）、`error`。

- 转写发生在推理 isolate（capturing 与 reasoning 之间），主线程不单独置
  `transcribing` 态（该枚举值保留给需要细分 UI 的场景）。
- **守卫**：reasoning+speaking 期间 `setSuppressed(true)`，避免把处理中的音频/自身
  播报当成新唤醒（防自激/重复触发）。用**抑制计数**叠加 TTS 自身的 push/pop。

## 5. 并发模型

- **主 isolate**：UI、门面、`IntentService`、`VoiceFeedback`。flutter_gemma 是异步
  （原生侧自带线程），留主 isolate 不阻塞。
- **推理 isolate**（`speech_worker`）：sherpa_onnx 是**同步 FFI**，必须隔离，否则阻塞
  UI/音频回调。`initBindings()` 在 isolate 内调用。
- **消息协议**（Map over SendPort）：
  - 主→worker：`{type:'init',config}` / `{type:'audio',samples:Float32List}` /
    `{type:'suppress',value}` / `{type:'unloadAsr'}` / `{type:'stop'}`
  - worker→主：`{type:'ready'}` / `{type:'wake',keyword}` / `{type:'vad',speaking}` /
    `{type:'transcript',text,language}` / `{type:'empty'}` / `{type:'error',message}`
  - `Float32List` 跨 isolate 可传输；音频量 16k 单声道≈32KB/s，开销可忽略。

## 6. 公开 API

```dart
SynlinkEngine({required SynlinkConfig config});
  Future<bool> isReady();
  Future<void> ensureModelsReady({void Function(SetupProgress)? onProgress});
  final CommandRegistry registry;                 // on(name,handler)/onUnknown
  void addTool(SynlinkTool tool);
  StreamSubscription<Command> onCommand(void Function(Command));
  Future<void> start(); Future<void> stop(); Future<void> dispose();
  Stream<PipelineState> stateStream;
  Stream<String> transcriptStream;
  Stream<Command> commandStream;
  Stream<Object> errorStream;

SynlinkConfig({
  required ModelSources modelSources,
  List<CommandDefinition> commands = const [],
  String wakeWord = 'hi synlink', double wakeWordThreshold = 0.25,
  WhisperModelSize whisperModel = small, String? asrLanguage,  // null=自动; tiny/base/small/medium
  PowerMode powerMode = balanced, bool pauseInBackground = true,
  Duration? keepModelsWarmIdle = 3min, Duration maxUtterance = 30s,
  Duration vadMinSilence = 600ms, VoiceFeedbackConfig voiceFeedback,
  int llmMaxTokens = 512, String? modelsDirectory,
});
  // 派生：inferenceThreads(1/2/4)、vadThreshold(0.6/0.5/0.4) 随 PowerMode

CommandDefinition({required String name, required String description,
  Map<String,dynamic> parameters = const {}, required CommandHandler handler});

Command{ String name; Map params; String transcript; String? language;
  bool get isUnknown; String? get target; }
```

## 7. 指令 / 意图设计

- 工具集 = `config.commands` 的工具 + 运行时 `addTool`；引擎把每个
  `CommandDefinition` 的 handler 注册到 `registry.on(name, handler)`。
- `buildSystemPrompt(tools)` 生成系统提示：列出工具与参数 schema，要求模型**只输出**
  `{"command":<名|none>,"params":{...},"language":<iso>}`；few‑shot 用通用样例（含从
  工具列表动态取的正例 + "none" 例），避免诱导出不存在的指令。
- `IntentService._extractJson` 做**括号平衡扫描**容错（代码块/多余文字），再
  `jsonDecode`；映射为 `Command`。语言以 LLM 输出为准，回退 Whisper 脚本启发式。
- **为何不用 flutter_gemma 原生 function‑call**：其 API 跨版本不稳；结构化 JSON 输出
  等价且更可移植，Qwen 对 JSON 很可靠。

## 8. 语音反馈设计

- `TtsEngine` 抽象；默认 `SystemTtsEngine`（flutter_tts）：
  `awaitSpeakCompletion(true)` 让 `speak()` 播完才返回；iOS 设
  `playAndRecord + duckOthers + defaultToSpeaker`，与录音共存；语种走 `bcp47For` +
  `isLanguageAvailable` 回退。
- 可选 `SherpaTtsEngine`（离线，需下载模型；`generate→writeWave→audioplayers`）。
- `VoiceFeedback`：选文案（按语种，回退英文）、`attachSuppression` 在每次播报前后
  push/pop 抑制唤醒；`override` 可让宿主完全接管。
- 触发：未识别（name 空）→ `speakNotRecognized`；异常 → `speakError`；可选成功播报。

## 9. 模型管理

- `ModelSources`：`fromBaseUrl(base)` 按 `<base>/<dir>/<file>` 规则生成全部 URL（CDN
  一行切换）；`defaults()` 为官方源。LLM 区分 `.task`(移动)/`.litertlm`(桌面)，
  `llmUrl` 按平台选。
- `DownloadManager`(dio)：逐文件下载到 `.part`（`Range` 续传，非 206 则重来）→
  SHA‑256 校验 → 原子改名；失败指数退避（2/4/8/16s）；`isBundleInstalled` 校验大小/SHA。
- LLM 由 flutter_gemma 自管下载（`GemmaBackend.ensureInstalled`）。
- **懒加载/卸载**：Whisper 首次 `transcribe` 才 `init`；LLM `complete` 时 `load`；
  `keepModelsWarmIdle` 到期 `unloadAsr` + LLM `unload`。
- 模型清单与官方 URL 见 `MODELS.md`；唤醒词 `keywords.txt` 用 sherpa `text2token` 生成。

## 10. 性能 / 省电

- **级联门控**：Silero VAD（极小，常跑）→ 仅说话时 `KWS.poll()` 解码（音频仍持续
  `feed` 保流连续）→ 命中才启 Whisper/LLM。
- **预滚动**：~300ms 环形缓冲，唤醒后预热 captureVad，避免切掉命令起音。
- int8 量化模型；recorder 直接 16k（免重采样）；100ms 分块；后台 isolate；
  `PowerMode` 调线程数/VAD 阈值；`pauseInBackground` 自动停。

## 11. 音频 / 麦克风冲突

- **单一持有者**：`AudioManager` 只开一路麦克风、广播给所有消费者；切阶段只改路由。
- iOS `playAndRecord` 让 TTS 与录音共存；Android 用 `voiceRecognition` 源（自带 AEC）；
  播报期间抑制唤醒防自激。
- 待补：来电/Siri 中断与路由变化（耳机/蓝牙）的订阅与自动恢复（见待办）。

## 12. 平台适配

- 权限：Android `RECORD_AUDIO`/`INTERNET`；iOS `NSMicrophoneUsageDescription`；
  macOS `device.audio-input` + `network.client`（Debug+Release 两个 entitlements）。
- 桌面 LLM：宿主 App 加 `flutter_gemma_litertlm` 并用 `.litertlm` 模型。
- 平台壳 + 权限由 `tool/bootstrap.sh` 一键生成/打补丁。

## 13. 依赖

| 包 | 版本 | 用途 | 备注 |
|----|------|------|------|
| sherpa_onnx | ^1.13.3 | KWS/VAD/ASR/可选TTS | 核对 config 字段 |
| flutter_gemma | ^0.9.0 | 本地 LLM | **版本/ API 需核对** |
| flutter_gemma_litertlm | ^0.9.0 | 桌面 LLM | 宿主 App 添加 |
| flutter_tts | ^4.2.5 | 系统 TTS | Linux 不支持 |
| record | ^5.1.2 | 麦克风 PCM | 核对 RecordConfig |
| audioplayers | ^6.1.0 | 离线 TTS 播放 | |
| dio/crypto/path/path_provider/permission_handler/meta | — | 下载/工具/权限 | |

## 14. 错误处理
- 统一经 `errorStream` 暴露；ASR/LLM 异常不崩链路，回到 listening 并可语音提示。
- 麦克风权限缺失 `start()` 抛 `StateError`；模型未就绪 `start()` 抛错并提示先
  `ensureModelsReady`。

## 15. 测试与验证
- 见 `VERIFY.md`：`flutter analyze` → 修 sherpa/gemma API 漂移 → 配模型 URL →
  四语种冒烟 → 四端权限。
- 建议补：`IntentService._extractJson` 与 `language.dart` 的单元测试（纯 Dart，易测）；
  `DownloadManager` 用本地 HTTP mock 测续传/校验。

## 16. 已知风险 / 待办

1. **flutter_gemma API**（`gemma_backend.dart`）：`createModel/createChat`、响应类型、
   安装方法、`ModelType` 取值需对当前版本核对。
2. **sherpa_onnx config 字段**与 `initBindings()` 名称核对；`record`/`audioplayers` API。
3. **模型 URL/文件名**确认（`model_sources.defaults()`、`MODELS.md`）；唤醒词 token 生成。
4. **音频中断/路由变化**的订阅与恢复未实现（来电、耳机插拔、设备切换）。
5. **离线 TTS** 生成走主 isolate；重负载可移入 isolate（参考 sherpa 官方示例）。
6. 暂无单元测试 / CI；建议加 GitHub Actions 跑 analyze + test。
7. 平台壳未入库（由 `bootstrap.sh` 生成），桌面需手动加 `flutter_gemma_litertlm`。

## 17. 扩展点
- 指令：`SynlinkConfig.commands` / `engine.addTool`。
- LLM：实现 `LlmBackend` 注入。
- TTS：`VoiceFeedback.override` 或实现 `TtsEngine`。
- 唤醒词：改 `wakeWord` + 重新生成 `keywords.txt`。
