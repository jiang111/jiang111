import 'dart:async';
import 'dart:io' show Platform;

import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter_tts/flutter_tts.dart';

import '../commands/command.dart';
import '../util/language.dart';

/// Which speech engine drives voice feedback.
enum TtsEngineType {
  /// OS built-in speech synthesiser via `flutter_tts`. No model download,
  /// multilingual out of the box. This is the default.
  system,

  /// sherpa-onnx fully-offline TTS (requires downloading a TTS model).
  /// Provided by [SherpaTtsEngine]; off by default to keep the app small.
  sherpaOffline,
}

/// How the feedback language is chosen.
enum FeedbackLanguage {
  /// Speak in the language detected from the user's utterance.
  matchUser,

  /// Always speak in [VoiceFeedbackConfig.fixedLanguage].
  fixed,
}

/// Default prompts spoken when a command could not be recognised.
const Map<String, String> _defaultNotRecognized = {
  'zh': '抱歉，没听懂指令，请再说一次。',
  'en': "Sorry, I didn't catch that command. Please try again.",
  'ja': 'すみません、コマンドを認識できませんでした。もう一度お願いします。',
  'es': 'Lo siento, no entendí el comando. Inténtalo de nuevo.',
};

const Map<String, String> _defaultError = {
  'zh': '出错了，请稍后再试。',
  'en': 'Something went wrong. Please try again.',
  'ja': 'エラーが発生しました。もう一度お試しください。',
  'es': 'Algo salió mal. Inténtalo de nuevo.',
};

/// Configuration for spoken feedback.
class VoiceFeedbackConfig {
  const VoiceFeedbackConfig({
    this.enabled = true,
    this.engine = TtsEngineType.system,
    this.languageMode = FeedbackLanguage.matchUser,
    this.fixedLanguage = 'en',
    this.announceOnSuccess = false,
    this.notRecognizedPrompts = _defaultNotRecognized,
    this.errorPrompts = _defaultError,
    this.successPrompts,
    this.speechRate = 0.5,
    this.volume = 1.0,
    this.pitch = 1.0,
  });

  final bool enabled;
  final TtsEngineType engine;
  final FeedbackLanguage languageMode;
  final String fixedLanguage;

  /// When true, also speak a short confirmation after a recognised command.
  final bool announceOnSuccess;

  /// lang-code → prompt for the "didn't catch that" case.
  final Map<String, String> notRecognizedPrompts;

  /// lang-code → prompt for error cases.
  final Map<String, String> errorPrompts;

  /// Optional per-command success prompts: `{commandName: {lang: text}}`.
  final Map<String, Map<String, String>>? successPrompts;

  final double speechRate;
  final double volume;
  final double pitch;

  VoiceFeedbackConfig copyWith({bool? enabled, TtsEngineType? engine}) =>
      VoiceFeedbackConfig(
        enabled: enabled ?? this.enabled,
        engine: engine ?? this.engine,
        languageMode: languageMode,
        fixedLanguage: fixedLanguage,
        announceOnSuccess: announceOnSuccess,
        notRecognizedPrompts: notRecognizedPrompts,
        errorPrompts: errorPrompts,
        successPrompts: successPrompts,
        speechRate: speechRate,
        volume: volume,
        pitch: pitch,
      );
}

/// Abstraction over a concrete TTS backend.
abstract class TtsEngine {
  Future<void> init();

  /// Speaks [text] and completes only after playback finishes, so the caller
  /// can safely resume microphone listening.
  Future<void> speak(String text, {String? language});

  Future<void> stop();
  Future<void> dispose();
}

/// Default engine backed by `flutter_tts` (the OS speech synthesiser).
class SystemTtsEngine implements TtsEngine {
  SystemTtsEngine({
    this.speechRate = 0.5,
    this.volume = 1.0,
    this.pitch = 1.0,
  });

  final double speechRate;
  final double volume;
  final double pitch;

  final FlutterTts _tts = FlutterTts();
  bool _inited = false;
  List<String>? _available;

  @override
  Future<void> init() async {
    if (_inited) return;
    // Make `speak()` resolve only when the utterance finishes (required on
    // Android, harmless elsewhere).
    await _tts.awaitSpeakCompletion(true);
    await _tts.setVolume(volume);
    await _tts.setSpeechRate(speechRate);
    await _tts.setPitch(pitch);

    if (!kIsWeb && Platform.isIOS) {
      // Coexist with the microphone recorder: play-and-record + duck others.
      await _tts.setIosAudioCategory(
        IosTextToSpeechAudioCategory.playAndRecord,
        [
          IosTextToSpeechAudioCategoryOptions.duckOthers,
          IosTextToSpeechAudioCategoryOptions.defaultToSpeaker,
          IosTextToSpeechAudioCategoryOptions.allowBluetooth,
        ],
        IosTextToSpeechAudioMode.voicePrompt,
      );
    }
    _inited = true;
  }

  @override
  Future<void> speak(String text, {String? language}) async {
    await init();
    if (language != null) {
      final locale = await _resolveLocale(language);
      if (locale != null) await _tts.setLanguage(locale);
    }
    await _tts.speak(text);
  }

  Future<String?> _resolveLocale(String lang) async {
    final wanted = bcp47For(lang);
    final available = await _languages();
    if (available == null || available.isEmpty) return wanted;

    // Exact match first.
    for (final l in available) {
      if (l.toLowerCase() == wanted.toLowerCase()) return l;
    }
    // Same base language (e.g. wanted zh-CN, device only has zh-TW).
    final base = wanted.toLowerCase().split('-').first;
    for (final l in available) {
      if (l.toLowerCase().startsWith('$base-') ||
          l.toLowerCase() == base) {
        return l;
      }
    }
    return null; // leave whatever the engine currently uses
  }

  Future<List<String>?> _languages() async {
    if (_available != null) return _available;
    try {
      final raw = await _tts.getLanguages;
      if (raw is List) {
        _available = raw.map((e) => e.toString()).toList();
      }
    } catch (_) {
      _available = null;
    }
    return _available;
  }

  @override
  Future<void> stop() => _tts.stop();

  @override
  Future<void> dispose() => _tts.stop();
}

/// Callback signature for fully overriding feedback (e.g. the host app wants
/// to use its own voice/audio stack).
typedef FeedbackOverride = Future<void> Function(String text, String? language);

/// High-level voice-feedback service used by the pipeline.
///
/// It resolves the right prompt + language, drives the engine, and — crucially
/// for the always-on microphone — brackets every utterance with
/// [onSpeakingChanged] so the caller can suppress wake-word detection while the
/// device is talking (avoiding self-triggering / echo).
class VoiceFeedback {
  VoiceFeedback({
    required this.config,
    TtsEngine? engine,
    this.onSpeakingChanged,
    this.override,
  }) : _engine = engine ?? SystemTtsEngine();

  final VoiceFeedbackConfig config;
  final TtsEngine _engine;

  /// Invoked with `true` just before speaking and `false` right after.
  final void Function(bool speaking)? onSpeakingChanged;

  /// If set, replaces the built-in engine entirely.
  final FeedbackOverride? override;

  void Function()? _onSuppressStart;
  void Function()? _onSuppressEnd;

  /// Wires push/pop callbacks invoked around every utterance so the caller can
  /// suppress wake-word detection while the device is talking (avoiding
  /// self-trigger / echo). Used by [VoicePipeline].
  void attachSuppression(void Function() onStart, void Function() onEnd) {
    _onSuppressStart = onStart;
    _onSuppressEnd = onEnd;
  }

  Future<void> init() async {
    if (!config.enabled) return;
    if (override == null) await _engine.init();
  }

  Future<void> speakNotRecognized(String? userLanguage) {
    final lang = _languageFor(userLanguage);
    return _say(resolvePrompt(config.notRecognizedPrompts, lang), lang);
  }

  Future<void> speakError(String? userLanguage) {
    final lang = _languageFor(userLanguage);
    return _say(resolvePrompt(config.errorPrompts, lang), lang);
  }

  Future<void> speakSuccess(Command command) {
    final prompts = config.successPrompts?[command.name];
    if (!config.announceOnSuccess || prompts == null) return Future.value();
    final lang = _languageFor(command.language);
    return _say(resolvePrompt(prompts, lang), lang);
  }

  /// Speaks an arbitrary message (host apps can call this directly).
  Future<void> say(String text, {String? language}) =>
      _say(text, _languageFor(language));

  String? _languageFor(String? userLanguage) {
    if (config.languageMode == FeedbackLanguage.fixed) {
      return config.fixedLanguage;
    }
    return userLanguage ?? config.fixedLanguage;
  }

  Future<void> _say(String text, String? language) async {
    if (!config.enabled || text.isEmpty) return;
    onSpeakingChanged?.call(true);
    _onSuppressStart?.call();
    try {
      if (override != null) {
        await override!(text, language);
      } else {
        await _engine.speak(text, language: language);
      }
    } finally {
      _onSuppressEnd?.call();
      onSpeakingChanged?.call(false);
    }
  }

  Future<void> stop() async {
    if (override == null) await _engine.stop();
  }

  Future<void> dispose() async {
    if (override == null) await _engine.dispose();
  }
}
