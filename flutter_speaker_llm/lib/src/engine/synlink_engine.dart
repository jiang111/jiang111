import 'dart:async';
import 'dart:io';

import 'package:flutter/widgets.dart';
import 'package:path/path.dart' as p;
import 'package:path_provider/path_provider.dart';

import '../audio/audio_manager.dart';
import '../commands/command.dart';
import '../commands/command_definition.dart';
import '../commands/registry.dart';
import '../download/download_manager.dart';
import '../download/model_asset.dart';
import '../infer/speech_engine.dart';
import '../llm/gemma_backend.dart';
import '../llm/intent_service.dart';
import '../llm/tools.dart';
import '../pipeline/voice_pipeline.dart';
import '../tts/sherpa_tts.dart';
import '../tts/voice_feedback.dart';
import 'synlink_config.dart';

/// Progress reported by [SynlinkEngine.ensureModelsReady].
class SetupProgress {
  const SetupProgress({required this.stage, required this.fraction});

  /// Which asset is downloading: `kws`, `vad`, `whisper`, `tts` or `llm`.
  final String stage;

  /// Progress in `[0, 1]` for [stage].
  final double fraction;

  @override
  String toString() =>
      'SetupProgress($stage ${(fraction * 100).toStringAsFixed(0)}%)';
}

/// The single public entry point of the library.
///
/// Lifecycle: construct → [ensureModelsReady] → register handlers via
/// [registry] / [onCommand] → [start]. Observe [stateStream],
/// [transcriptStream] and [commandStream].
class SynlinkEngine with WidgetsBindingObserver {
  SynlinkEngine({required this.config});

  final SynlinkConfig config;

  /// Direct access to the dispatcher. Handlers from `config.commands` are
  /// registered automatically; use `registry.on(name, handler)` for extras and
  /// `registry.onUnknown(...)` for a fallback.
  final CommandRegistry registry = CommandRegistry();

  final DownloadManager _downloads = DownloadManager();
  final List<SynlinkTool> _extraTools = [];

  final StreamController<PipelineState> _stateCtrl =
      StreamController<PipelineState>.broadcast();
  final StreamController<String> _transcriptCtrl =
      StreamController<String>.broadcast();
  final StreamController<Command> _commandCtrl =
      StreamController<Command>.broadcast();
  final StreamController<Object> _errorCtrl =
      StreamController<Object>.broadcast();

  Stream<PipelineState> get stateStream => _stateCtrl.stream;
  Stream<String> get transcriptStream => _transcriptCtrl.stream;
  Stream<Command> get commandStream => _commandCtrl.stream;
  Stream<Object> get errorStream => _errorCtrl.stream;

  Directory? _modelsDir;
  GemmaBackend? _llm;
  IntentService? _intent;
  VoiceFeedback? _feedback;
  VoicePipeline? _pipeline;
  final List<StreamSubscription<dynamic>> _subs = [];
  bool _observerAdded = false;

  /// Convenience: listen to every recognised command.
  StreamSubscription<Command> onCommand(void Function(Command) handler) =>
      commandStream.listen(handler);

  /// Adds a tool the LLM may call. Register its handler with
  /// `registry.on(tool.name, ...)`.
  void addTool(SynlinkTool tool) {
    _extraTools.add(tool);
    _intent?.addTool(tool);
  }

  Future<Directory> _ensureModelsDir() async {
    if (_modelsDir != null) return _modelsDir!;
    final base = config.modelsDirectory != null
        ? Directory(config.modelsDirectory!)
        : Directory(p.join(
            (await getApplicationDocumentsDirectory()).path,
            'synlink_models',
          ));
    await base.create(recursive: true);
    return _modelsDir = base;
  }

  List<ModelBundle> _sherpaBundles() {
    final src = config.modelSources;
    final whisper = config.whisperModel == WhisperModelSize.tiny
        ? src.whisperTiny
        : src.whisperBase;
    final bundles = <ModelBundle>[src.kws, src.vad, whisper];
    if (config.voiceFeedback.engine == TtsEngineType.sherpaOffline &&
        src.tts != null) {
      bundles.add(src.tts!);
    }
    return bundles;
  }

  /// Whether every required model is already present.
  Future<bool> isReady() async {
    final dir = await _ensureModelsDir();
    for (final b in _sherpaBundles()) {
      if (!await _downloads.isBundleInstalled(b, dir)) return false;
    }
    _llm ??= GemmaBackend(
      modelUrl: config.modelSources.llmUrl,
      maxTokens: config.llmMaxTokens,
    );
    return _llm!.isInstalled();
  }

  /// Downloads/verifies all models. Call once before [start].
  Future<void> ensureModelsReady({
    void Function(SetupProgress)? onProgress,
  }) async {
    final dir = await _ensureModelsDir();
    for (final b in _sherpaBundles()) {
      await _downloads.downloadBundle(
        b,
        dir,
        onProgress: (dp) => onProgress
            ?.call(SetupProgress(stage: b.id, fraction: dp.overallFraction)),
      );
    }
    _llm ??= GemmaBackend(
      modelUrl: config.modelSources.llmUrl,
      maxTokens: config.llmMaxTokens,
    );
    await _llm!.ensureInstalled(
      onProgress: (f) => onProgress?.call(SetupProgress(stage: 'llm', fraction: f)),
    );
  }

  SpeechConfig _buildSpeechConfig(Directory dir) {
    final src = config.modelSources;
    final isTiny = config.whisperModel == WhisperModelSize.tiny;
    final whisper = isTiny ? src.whisperTiny : src.whisperBase;
    final encName = isTiny ? 'tiny-encoder.int8.onnx' : 'base-encoder.int8.onnx';
    final decName = isTiny ? 'tiny-decoder.int8.onnx' : 'base-decoder.int8.onnx';
    final tokName = isTiny ? 'tiny-tokens.txt' : 'base-tokens.txt';
    String path(ModelBundle b, String name) => _downloads.filePath(b, dir, name);

    return SpeechConfig(
      kwsEncoder: path(src.kws, 'encoder.onnx'),
      kwsDecoder: path(src.kws, 'decoder.onnx'),
      kwsJoiner: path(src.kws, 'joiner.onnx'),
      kwsTokens: path(src.kws, 'tokens.txt'),
      keywordsFile: path(src.kws, 'keywords.txt'),
      vadModel: path(src.vad, 'silero_vad.onnx'),
      whisperEncoder: path(whisper, encName),
      whisperDecoder: path(whisper, decName),
      whisperTokens: path(whisper, tokName),
      asrLanguage: config.asrLanguage ?? '',
      numThreads: config.inferenceThreads,
      vadThreshold: config.vadThreshold,
      minSilence: config.vadMinSilence.inMilliseconds / 1000.0,
      wakeThreshold: config.wakeWordThreshold,
      maxUtteranceSeconds: config.maxUtterance.inMilliseconds / 1000.0,
    );
  }

  TtsEngine? _buildTtsEngine(Directory dir) {
    final src = config.modelSources;
    if (config.voiceFeedback.engine == TtsEngineType.sherpaOffline &&
        src.tts != null) {
      final tts = src.tts!;
      return SherpaTtsEngine(
        paths: SherpaTtsModelPaths(
          model: _downloads.filePath(tts, dir, 'model.onnx'),
          tokens: _downloads.filePath(tts, dir, 'tokens.txt'),
          lexicon: _downloads.filePath(tts, dir, 'lexicon.txt'),
        ),
        cacheDir: Directory(p.join(dir.path, 'tts_cache')),
      );
    }
    return null; // VoiceFeedback defaults to the system TTS engine.
  }

  /// Starts always-on listening. Throws if models aren't downloaded yet.
  Future<void> start() async {
    if (_pipeline != null) return;
    WidgetsFlutterBinding.ensureInitialized();
    if (!await isReady()) {
      throw StateError(
          'Models are not ready. Call ensureModelsReady() before start().');
    }
    final dir = await _ensureModelsDir();

    _llm ??= GemmaBackend(
      modelUrl: config.modelSources.llmUrl,
      maxTokens: config.llmMaxTokens,
    );

    // Assemble the tool set the LLM sees: the integrator's configured commands
    // plus any added at runtime. Register their handlers with the dispatcher.
    final tools = <SynlinkTool>[
      for (final CommandDefinition d in config.commands) d.toTool(),
      ..._extraTools,
    ];
    for (final CommandDefinition d in config.commands) {
      registry.on(d.name, d.handler);
    }
    _intent = IntentService(backend: _llm!, tools: tools);

    _feedback = VoiceFeedback(
      config: config.voiceFeedback,
      engine: _buildTtsEngine(dir),
    );

    final pipeline = VoicePipeline(
      audio: AudioManager(),
      speech: SpeechEngine(),
      speechConfig: _buildSpeechConfig(dir),
      intent: _intent!,
      registry: registry,
      feedback: _feedback!,
      keepModelsWarmIdle: config.keepModelsWarmIdle,
    );
    _pipeline = pipeline;

    _subs
      ..add(pipeline.stateStream.listen(_stateCtrl.add))
      ..add(pipeline.transcriptStream.listen(_transcriptCtrl.add))
      ..add(pipeline.commandStream.listen(_commandCtrl.add))
      ..add(pipeline.errorStream.listen(_errorCtrl.add));

    if (config.pauseInBackground && !_observerAdded) {
      WidgetsBinding.instance.addObserver(this);
      _observerAdded = true;
    }

    await pipeline.start();
  }

  Future<void> stop() async {
    await _pipeline?.stop();
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (!config.pauseInBackground) return;
    switch (state) {
      case AppLifecycleState.resumed:
        unawaited(_pipeline?.resume() ?? Future.value());
      case AppLifecycleState.inactive:
      case AppLifecycleState.paused:
      case AppLifecycleState.hidden:
      case AppLifecycleState.detached:
        unawaited(_pipeline?.pause() ?? Future.value());
    }
  }

  Future<void> dispose() async {
    if (_observerAdded) {
      WidgetsBinding.instance.removeObserver(this);
      _observerAdded = false;
    }
    for (final s in _subs) {
      await s.cancel();
    }
    _subs.clear();
    await _pipeline?.dispose();
    _pipeline = null;
    await _llm?.dispose();
    await _stateCtrl.close();
    await _transcriptCtrl.close();
    await _commandCtrl.close();
    await _errorCtrl.close();
  }
}
