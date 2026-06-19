import 'dart:typed_data';

import 'package:sherpa_onnx/sherpa_onnx.dart' as sherpa;

import '../audio/pcm.dart';
import '../util/language.dart';

/// Result of a transcription.
class AsrResult {
  const AsrResult({required this.text, this.language});

  final String text;

  /// Best-effort language guess (script-based). The LLM provides the
  /// authoritative language used for voice feedback.
  final String? language;
}

/// sherpa-onnx offline Whisper recogniser (multilingual). Runs synchronous
/// FFI — drive it from the inference isolate. Loaded lazily and freed on idle
/// to save memory.
class WhisperAsr {
  WhisperAsr({
    required this.encoder,
    required this.decoder,
    required this.tokens,
    this.language = '',
    this.numThreads = 2,
  });

  final String encoder;
  final String decoder;
  final String tokens;

  /// Forced language (ISO-639-1) or empty for auto-detect.
  final String language;
  final int numThreads;

  sherpa.OfflineRecognizer? _recognizer;

  bool get isLoaded => _recognizer != null;

  void init() {
    if (_recognizer != null) return;
    final model = sherpa.OfflineModelConfig(
      whisper: sherpa.OfflineWhisperModelConfig(
        encoder: encoder,
        decoder: decoder,
        language: language,
        task: 'transcribe',
        tailPaddings: -1,
      ),
      tokens: tokens,
      numThreads: numThreads,
      provider: 'cpu',
      debug: false,
      modelType: 'whisper',
    );
    _recognizer = sherpa.OfflineRecognizer(
      sherpa.OfflineRecognizerConfig(
        model: model,
        decodingMethod: 'greedy_search',
      ),
    );
  }

  AsrResult transcribe(Float32List samples) {
    init();
    final recognizer = _recognizer!;
    final stream = recognizer.createStream();
    stream.acceptWaveform(samples: samples, sampleRate: kSampleRate);
    recognizer.decode(stream);
    final text = recognizer.getResult(stream).text.trim();
    stream.free();
    return AsrResult(text: text, language: detectLanguageHeuristic(text));
  }

  void free() {
    _recognizer?.free();
    _recognizer = null;
  }
}
