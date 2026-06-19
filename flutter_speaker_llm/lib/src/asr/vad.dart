import 'dart:typed_data';

import 'package:sherpa_onnx/sherpa_onnx.dart' as sherpa;

import '../audio/pcm.dart';

/// Thin wrapper around sherpa-onnx Silero VAD.
///
/// Two jobs: (1) a cheap "is anyone speaking" gate so the heavier keyword
/// spotter only runs on speech, and (2) endpointing — emitting a finished
/// utterance once the user stops talking.
///
/// Runs synchronous FFI; construct and drive it inside the inference isolate.
class Vad {
  Vad({
    required this.modelPath,
    this.threshold = 0.5,
    this.minSilence = 0.6,
    this.minSpeech = 0.25,
    this.maxSpeech = 30.0,
    this.numThreads = 1,
    this.bufferSizeInSeconds = 30,
  });

  final String modelPath;
  final double threshold;
  final double minSilence;
  final double minSpeech;
  final double maxSpeech;
  final int numThreads;
  final int bufferSizeInSeconds;

  sherpa.VoiceActivityDetector? _vad;

  void init() {
    if (_vad != null) return;
    final config = sherpa.VadModelConfig(
      sileroVad: sherpa.SileroVadModelConfig(
        model: modelPath,
        threshold: threshold,
        minSilenceDuration: minSilence,
        minSpeechDuration: minSpeech,
        maxSpeechDuration: maxSpeech,
        windowSize: 512,
      ),
      numThreads: numThreads,
      sampleRate: kSampleRate,
      provider: 'cpu',
      debug: false,
    );
    _vad = sherpa.VoiceActivityDetector(
      config: config,
      bufferSizeInSeconds: bufferSizeInSeconds.toDouble(),
    );
  }

  void acceptWaveform(Float32List samples) => _vad!.acceptWaveform(samples);

  /// Whether speech is currently in progress.
  bool get isSpeaking => _vad!.isDetected();

  /// Whether a completed utterance is queued.
  bool get hasSegment => !_vad!.isEmpty();

  /// Pops the next finished utterance's samples.
  Float32List popSegment() {
    final segment = _vad!.front();
    _vad!.pop();
    return segment.samples;
  }

  /// Forces the current speech (if any) to be finalised.
  void flush() => _vad!.flush();

  /// Clears all buffered audio and state.
  void reset() => _vad!.clear();

  void free() {
    _vad?.free();
    _vad = null;
  }
}
