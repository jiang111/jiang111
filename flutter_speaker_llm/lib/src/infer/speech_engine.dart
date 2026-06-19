import 'dart:async';
import 'dart:isolate';
import 'dart:typed_data';

import 'speech_worker.dart';

/// Everything the inference isolate needs to build its models. Only primitives
/// and strings so it crosses the isolate boundary cleanly.
class SpeechConfig {
  const SpeechConfig({
    required this.kwsEncoder,
    required this.kwsDecoder,
    required this.kwsJoiner,
    required this.kwsTokens,
    required this.keywordsFile,
    required this.vadModel,
    required this.whisperEncoder,
    required this.whisperDecoder,
    required this.whisperTokens,
    this.asrLanguage = '',
    this.numThreads = 2,
    this.vadThreshold = 0.5,
    this.minSilence = 0.6,
    this.wakeThreshold = 0.25,
    this.wakeScore = 1.0,
    this.maxUtteranceSeconds = 30.0,
  });

  final String kwsEncoder;
  final String kwsDecoder;
  final String kwsJoiner;
  final String kwsTokens;
  final String keywordsFile;
  final String vadModel;
  final String whisperEncoder;
  final String whisperDecoder;
  final String whisperTokens;
  final String asrLanguage;
  final int numThreads;
  final double vadThreshold;
  final double minSilence;
  final double wakeThreshold;
  final double wakeScore;
  final double maxUtteranceSeconds;

  Map<String, dynamic> toMap() => {
        'kwsEncoder': kwsEncoder,
        'kwsDecoder': kwsDecoder,
        'kwsJoiner': kwsJoiner,
        'kwsTokens': kwsTokens,
        'keywordsFile': keywordsFile,
        'vadModel': vadModel,
        'whisperEncoder': whisperEncoder,
        'whisperDecoder': whisperDecoder,
        'whisperTokens': whisperTokens,
        'asrLanguage': asrLanguage,
        'numThreads': numThreads,
        'vadThreshold': vadThreshold,
        'minSilence': minSilence,
        'wakeThreshold': wakeThreshold,
        'wakeScore': wakeScore,
        'maxUtteranceSeconds': maxUtteranceSeconds,
      };

  static SpeechConfig fromMap(Map<dynamic, dynamic> m) => SpeechConfig(
        kwsEncoder: m['kwsEncoder'] as String,
        kwsDecoder: m['kwsDecoder'] as String,
        kwsJoiner: m['kwsJoiner'] as String,
        kwsTokens: m['kwsTokens'] as String,
        keywordsFile: m['keywordsFile'] as String,
        vadModel: m['vadModel'] as String,
        whisperEncoder: m['whisperEncoder'] as String,
        whisperDecoder: m['whisperDecoder'] as String,
        whisperTokens: m['whisperTokens'] as String,
        asrLanguage: m['asrLanguage'] as String? ?? '',
        numThreads: m['numThreads'] as int? ?? 2,
        vadThreshold: (m['vadThreshold'] as num?)?.toDouble() ?? 0.5,
        minSilence: (m['minSilence'] as num?)?.toDouble() ?? 0.6,
        wakeThreshold: (m['wakeThreshold'] as num?)?.toDouble() ?? 0.25,
        wakeScore: (m['wakeScore'] as num?)?.toDouble() ?? 1.0,
        maxUtteranceSeconds:
            (m['maxUtteranceSeconds'] as num?)?.toDouble() ?? 30.0,
      );
}

/// Events emitted by the inference isolate.
sealed class SpeechEvent {
  const SpeechEvent();
}

class SpeechReady extends SpeechEvent {
  const SpeechReady();
}

class WakeDetected extends SpeechEvent {
  const WakeDetected(this.keyword);
  final String keyword;
}

class VadStateEvent extends SpeechEvent {
  const VadStateEvent(this.speaking);
  final bool speaking;
}

class TranscriptEvent extends SpeechEvent {
  const TranscriptEvent(this.text, this.language);
  final String text;
  final String? language;
}

class CaptureEmpty extends SpeechEvent {
  const CaptureEmpty();
}

class SpeechError extends SpeechEvent {
  const SpeechError(this.message);
  final String message;
}

SpeechEvent _decodeEvent(Map<dynamic, dynamic> m) {
  switch (m['type'] as String) {
    case 'ready':
      return const SpeechReady();
    case 'wake':
      return WakeDetected(m['keyword'] as String? ?? '');
    case 'vad':
      return VadStateEvent(m['speaking'] as bool? ?? false);
    case 'transcript':
      return TranscriptEvent(
          m['text'] as String? ?? '', m['language'] as String?);
    case 'empty':
      return const CaptureEmpty();
    case 'error':
    default:
      return SpeechError(m['message'] as String? ?? 'unknown error');
  }
}

/// Main-isolate handle to the speech inference isolate. Push audio in, listen
/// to [events] out. Wake detection can be temporarily suppressed (e.g. while
/// speaking TTS feedback) via [setSuppressed].
class SpeechEngine {
  final StreamController<SpeechEvent> _events =
      StreamController<SpeechEvent>.broadcast();
  Isolate? _isolate;
  SendPort? _toWorker;
  ReceivePort? _fromWorker;

  Stream<SpeechEvent> get events => _events.stream;

  bool get isRunning => _isolate != null;

  Future<void> start(SpeechConfig config) async {
    if (_isolate != null) return;
    final ready = Completer<void>();
    _fromWorker = ReceivePort();
    _fromWorker!.listen((dynamic msg) {
      if (msg is SendPort) {
        _toWorker = msg;
        _toWorker!.send({'type': 'init', 'config': config.toMap()});
        return;
      }
      if (msg is Map) {
        final event = _decodeEvent(msg);
        if (event is SpeechReady && !ready.isCompleted) ready.complete();
        if (event is SpeechError && !ready.isCompleted) {
          ready.completeError(StateError(event.message));
        }
        if (!_events.isClosed) _events.add(event);
      }
    });
    _isolate = await Isolate.spawn(speechWorkerEntry, _fromWorker!.sendPort);
    await ready.future.timeout(const Duration(seconds: 60));
  }

  void pushAudio(Float32List samples) =>
      _toWorker?.send({'type': 'audio', 'samples': samples});

  void setSuppressed(bool suppressed) =>
      _toWorker?.send({'type': 'suppress', 'value': suppressed});

  /// Asks the worker to unload the heavy Whisper model to save memory.
  void unloadAsr() => _toWorker?.send({'type': 'unloadAsr'});

  Future<void> stop() async {
    _toWorker?.send({'type': 'stop'});
    await Future<void>.delayed(const Duration(milliseconds: 50));
    _isolate?.kill(priority: Isolate.immediate);
    _isolate = null;
    _toWorker = null;
    _fromWorker?.close();
    _fromWorker = null;
  }

  Future<void> dispose() async {
    await stop();
    await _events.close();
  }
}
