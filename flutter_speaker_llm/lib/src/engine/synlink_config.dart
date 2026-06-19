import '../commands/command_definition.dart';
import '../download/model_sources.dart';
import '../tts/voice_feedback.dart';

/// Trades microphone sensitivity against battery/CPU use.
enum PowerMode {
  /// Most battery-friendly: stricter VAD gate, fewer inference threads.
  low,

  /// Sensible default.
  balanced,

  /// Most responsive / lowest latency, highest power use.
  performance,
}

/// Whisper model size used for transcription. Larger = more accurate but
/// bigger/slower. `small` is the recommended default for command recognition
/// (notably better than tiny/base, still fully multilingual incl. Spanish).
enum WhisperModelSize { tiny, base, small, medium }

/// Top-level configuration for [SynlinkEngine].
class SynlinkConfig {
  const SynlinkConfig({
    required this.modelSources,
    this.commands = const [],
    this.wakeWord = 'hi synlink',
    this.wakeWordThreshold = 0.25,
    this.whisperModel = WhisperModelSize.small,
    this.asrLanguage,
    this.powerMode = PowerMode.balanced,
    this.pauseInBackground = true,
    this.keepModelsWarmIdle = const Duration(minutes: 3),
    this.maxUtterance = const Duration(seconds: 30),
    this.vadMinSilence = const Duration(milliseconds: 600),
    this.voiceFeedback = const VoiceFeedbackConfig(),
    this.llmMaxTokens = 512,
    this.modelsDirectory,
  });

  /// Where to download model files from. Use [ModelSources.fromBaseUrl] to
  /// point at your CDN.
  final ModelSources modelSources;

  /// The app's commands (recognition + handler in one place). The library ships
  /// none — define your own here. These are registered with the LLM and the
  /// dispatcher automatically.
  final List<CommandDefinition> commands;

  /// Wake phrase. Must match the entry in the KWS `keywords.txt` (English
  /// tokeniser). Default: `hi synlink`.
  final String wakeWord;

  /// Keyword-spotter trigger threshold (lower = more sensitive).
  final double wakeWordThreshold;

  final WhisperModelSize whisperModel;

  /// ISO-639-1 language to force for transcription, or `null` to let Whisper
  /// auto-detect (recommended for multilingual use).
  final String? asrLanguage;

  final PowerMode powerMode;

  /// Stop listening while the app is in the background.
  final bool pauseInBackground;

  /// Unload the heavy Whisper/LLM models after this much idle time to free
  /// memory. `null` keeps them resident.
  final Duration? keepModelsWarmIdle;

  /// Hard cap on a single utterance, bounding memory and compute.
  final Duration maxUtterance;

  /// Silence needed to consider an utterance finished (VAD endpointing).
  final Duration vadMinSilence;

  final VoiceFeedbackConfig voiceFeedback;

  /// Max tokens the LLM may generate per turn.
  final int llmMaxTokens;

  /// Override the directory models are stored in. Defaults to the app's
  /// documents directory.
  final String? modelsDirectory;

  /// Threads for sherpa-onnx inference, derived from [powerMode].
  int get inferenceThreads => switch (powerMode) {
        PowerMode.low => 1,
        PowerMode.balanced => 2,
        PowerMode.performance => 4,
      };

  /// VAD speech-probability threshold, derived from [powerMode].
  double get vadThreshold => switch (powerMode) {
        PowerMode.low => 0.6,
        PowerMode.balanced => 0.5,
        PowerMode.performance => 0.4,
      };

  SynlinkConfig copyWith({
    ModelSources? modelSources,
    String? wakeWord,
    WhisperModelSize? whisperModel,
    PowerMode? powerMode,
    VoiceFeedbackConfig? voiceFeedback,
  }) {
    return SynlinkConfig(
      modelSources: modelSources ?? this.modelSources,
      wakeWord: wakeWord ?? this.wakeWord,
      wakeWordThreshold: wakeWordThreshold,
      whisperModel: whisperModel ?? this.whisperModel,
      asrLanguage: asrLanguage,
      powerMode: powerMode ?? this.powerMode,
      pauseInBackground: pauseInBackground,
      keepModelsWarmIdle: keepModelsWarmIdle,
      maxUtterance: maxUtterance,
      vadMinSilence: vadMinSilence,
      voiceFeedback: voiceFeedback ?? this.voiceFeedback,
      llmMaxTokens: llmMaxTokens,
      modelsDirectory: modelsDirectory,
    );
  }
}
