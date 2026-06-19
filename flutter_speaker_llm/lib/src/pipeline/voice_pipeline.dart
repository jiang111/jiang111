import 'dart:async';

import '../audio/audio_manager.dart';
import '../commands/command.dart';
import '../commands/registry.dart';
import '../infer/speech_engine.dart';
import '../llm/intent_service.dart';
import '../tts/voice_feedback.dart';

/// High-level state of the voice pipeline, surfaced for UI.
enum PipelineState {
  idle,
  listening,
  capturing,
  transcribing,
  reasoning,
  speaking,
  paused,
  error,
}

/// Orchestrates the full flow: microphone → wake word → VAD endpointing →
/// Whisper → LLM intent → command dispatch / voice feedback.
class VoicePipeline {
  VoicePipeline({
    required AudioManager audio,
    required SpeechEngine speech,
    required SpeechConfig speechConfig,
    required IntentService intent,
    required CommandRegistry registry,
    required VoiceFeedback feedback,
    this.keepModelsWarmIdle,
  })  : _audio = audio,
        _speech = speech,
        _speechConfig = speechConfig,
        _intent = intent,
        _registry = registry,
        _feedback = feedback {
    // Suppressing wake detection while the device speaks avoids self-triggering.
    _feedback.attachSuppression(_pushSuppress, _popSuppress);
  }

  final AudioManager _audio;
  final SpeechEngine _speech;
  final SpeechConfig _speechConfig;
  final IntentService _intent;
  final CommandRegistry _registry;
  final VoiceFeedback _feedback;

  /// Unload heavy models after this idle period. `null` keeps them resident.
  final Duration? keepModelsWarmIdle;

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

  StreamSubscription<dynamic>? _audioSub;
  StreamSubscription<SpeechEvent>? _speechSub;
  Timer? _idleTimer;
  int _suppressDepth = 0;
  bool _started = false;
  PipelineState _state = PipelineState.idle;

  PipelineState get state => _state;

  void _setState(PipelineState s) {
    _state = s;
    if (!_stateCtrl.isClosed) _stateCtrl.add(s);
  }

  void _pushSuppress() {
    _suppressDepth++;
    _speech.setSuppressed(true);
  }

  void _popSuppress() {
    if (_suppressDepth > 0) _suppressDepth--;
    if (_suppressDepth == 0) _speech.setSuppressed(false);
  }

  Future<void> start() async {
    if (_started) return;
    _started = true;
    await _feedback.init();
    await _speech.start(_speechConfig);
    await _audio.start();
    _audioSub = _audio.samples.listen(
      _speech.pushAudio,
      onError: (Object e) => _errorCtrl.add(e),
    );
    _speechSub = _speech.events.listen(_onSpeechEvent);
    _setState(PipelineState.listening);
  }

  void _onSpeechEvent(SpeechEvent event) {
    switch (event) {
      case SpeechReady():
        break;
      case WakeDetected():
        _bumpIdle();
        _setState(PipelineState.capturing);
      case VadStateEvent():
        break;
      case CaptureEmpty():
        // Woke up but heard no usable speech — just resume listening.
        _setState(PipelineState.listening);
      case TranscriptEvent(text: final text, language: final language):
        unawaited(_handleTranscript(text, language));
      case SpeechError(message: final message):
        _errorCtrl.add(message);
        _setState(PipelineState.listening);
    }
  }

  Future<void> _handleTranscript(String text, String? language) async {
    _bumpIdle();
    if (!_transcriptCtrl.isClosed) _transcriptCtrl.add(text);
    _setState(PipelineState.reasoning);
    // Don't let the command/feedback phase re-trigger the wake word.
    _pushSuppress();
    try {
      final command = await _intent.recognize(text, fallbackLanguage: language);
      if (!_commandCtrl.isClosed) _commandCtrl.add(command);

      final handled = await _registry.dispatch(command);

      if (command.name.isEmpty) {
        // Nothing matched → tell the user.
        _setState(PipelineState.speaking);
        await _feedback.speakNotRecognized(command.language);
      } else if (handled) {
        _setState(PipelineState.speaking);
        await _feedback.speakSuccess(command);
      }
    } catch (e) {
      _errorCtrl.add(e);
      _setState(PipelineState.speaking);
      await _feedback.speakError(language);
    } finally {
      _popSuppress();
      _setState(PipelineState.listening);
    }
  }

  void _bumpIdle() {
    if (keepModelsWarmIdle == null) return;
    _idleTimer?.cancel();
    _idleTimer = Timer(keepModelsWarmIdle!, () {
      // Free RAM after inactivity; models reload lazily on next use.
      _speech.unloadAsr();
      unawaited(_intent.unload());
    });
  }

  /// Temporarily stop listening (e.g. app backgrounded).
  Future<void> pause() async {
    if (!_started || _state == PipelineState.paused) return;
    await _audioSub?.cancel();
    _audioSub = null;
    await _audio.stop();
    _setState(PipelineState.paused);
  }

  /// Resume listening after [pause].
  Future<void> resume() async {
    if (!_started || _state != PipelineState.paused) return;
    await _audio.start();
    _audioSub = _audio.samples.listen(
      _speech.pushAudio,
      onError: (Object e) => _errorCtrl.add(e),
    );
    _setState(PipelineState.listening);
  }

  Future<void> stop() async {
    _idleTimer?.cancel();
    await _audioSub?.cancel();
    _audioSub = null;
    await _speechSub?.cancel();
    _speechSub = null;
    await _audio.stop();
    await _speech.stop();
    _started = false;
    _setState(PipelineState.idle);
  }

  Future<void> dispose() async {
    await stop();
    await _speech.dispose();
    await _audio.dispose();
    await _feedback.dispose();
    await _stateCtrl.close();
    await _transcriptCtrl.close();
    await _commandCtrl.close();
    await _errorCtrl.close();
  }
}
