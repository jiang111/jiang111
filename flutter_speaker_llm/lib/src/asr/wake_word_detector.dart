import 'dart:typed_data';

import 'package:sherpa_onnx/sherpa_onnx.dart' as sherpa;

import '../audio/pcm.dart';

/// sherpa-onnx streaming keyword spotter used to detect the wake phrase
/// (default "hi synlink"). Runs synchronous FFI — drive it from the inference
/// isolate.
class WakeWordDetector {
  WakeWordDetector({
    required this.encoder,
    required this.decoder,
    required this.joiner,
    required this.tokens,
    required this.keywordsFile,
    this.keywordsThreshold = 0.25,
    this.keywordsScore = 1.0,
    this.numThreads = 1,
  });

  final String encoder;
  final String decoder;
  final String joiner;
  final String tokens;

  /// Path to the keywords file containing the tokenised wake phrase.
  final String keywordsFile;
  final double keywordsThreshold;
  final double keywordsScore;
  final int numThreads;

  sherpa.KeywordSpotter? _spotter;
  sherpa.OnlineStream? _stream;

  void init() {
    if (_spotter != null) return;
    final model = sherpa.OnlineModelConfig(
      transducer: sherpa.OnlineTransducerModelConfig(
        encoder: encoder,
        decoder: decoder,
        joiner: joiner,
      ),
      tokens: tokens,
      numThreads: numThreads,
      provider: 'cpu',
      debug: false,
    );
    final config = sherpa.KeywordSpotterConfig(
      model: model,
      keywordsFile: keywordsFile,
      keywordsScore: keywordsScore,
      keywordsThreshold: keywordsThreshold,
      maxActivePaths: 4,
    );
    _spotter = sherpa.KeywordSpotter(config);
    _stream = _spotter!.createStream();
  }

  /// Feeds [samples] into the keyword-spotter stream. Call this on every audio
  /// chunk to keep the stream continuous (the KWS model expects gap-free
  /// audio), even when [poll] is gated by VAD.
  void feed(Float32List samples) {
    _stream!.acceptWaveform(samples: samples, sampleRate: kSampleRate);
  }

  /// Decodes any buffered audio and returns the matched keyword, or `null`.
  /// Decoding is the expensive step, so callers may gate it behind a VAD
  /// "is speaking" check to save power during silence — feeding stays
  /// continuous, only decoding is skipped.
  String? poll() {
    final spotter = _spotter!;
    final stream = _stream!;
    while (spotter.isReady(stream)) {
      spotter.decode(stream);
    }
    final result = spotter.getResult(stream);
    if (result.keyword.isNotEmpty) {
      // Reset so the same audio tail doesn't re-trigger.
      spotter.reset(stream);
      return result.keyword;
    }
    return null;
  }

  /// Convenience: [feed] then [poll].
  String? accept(Float32List samples) {
    feed(samples);
    return poll();
  }

  void free() {
    _stream?.free();
    _spotter?.free();
    _stream = null;
    _spotter = null;
  }
}
