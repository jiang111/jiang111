import 'dart:isolate';
import 'dart:typed_data';

import 'package:sherpa_onnx/sherpa_onnx.dart' as sherpa;

import '../asr/vad.dart';
import '../asr/wake_word_detector.dart';
import '../asr/whisper_asr.dart';
import '../audio/pcm.dart';
import '../audio/ring_buffer.dart';
import 'speech_engine.dart';

enum _State { listeningWake, capturing }

/// Isolate entry point. Owns all sherpa-onnx FFI objects so their synchronous
/// inference never blocks the UI isolate.
void speechWorkerEntry(SendPort toMain) {
  final fromMain = ReceivePort();
  toMain.send(fromMain.sendPort);

  _Worker? worker;
  fromMain.listen((dynamic msg) {
    if (msg is! Map) return;
    switch (msg['type'] as String) {
      case 'init':
        try {
          worker = _Worker(toMain, SpeechConfig.fromMap(msg['config'] as Map))
            ..init();
          toMain.send({'type': 'ready'});
        } catch (e) {
          toMain.send({'type': 'error', 'message': 'init failed: $e'});
        }
      case 'audio':
        worker?.onAudio(msg['samples'] as Float32List);
      case 'suppress':
        worker?.suppressed = msg['value'] as bool;
      case 'unloadAsr':
        worker?.unloadAsr();
      case 'stop':
        worker?.dispose();
        fromMain.close();
    }
  });
}

class _Worker {
  _Worker(this.toMain, this.config);

  final SendPort toMain;
  final SpeechConfig config;

  late final Vad _gateVad;
  late final Vad _captureVad;
  late final WakeWordDetector _wake;
  late final WhisperAsr _asr;

  // ~300 ms pre-roll so a command spoken immediately after "hi synlink" isn't
  // clipped.
  final Float32RingBuffer _preroll = Float32RingBuffer(kSampleRate ~/ 3);

  _State _state = _State.listeningWake;
  bool suppressed = false;
  int _capturedSamples = 0;
  bool _sawSpeech = false;
  int _maxCaptureSamples = 0;
  bool _wasSpeaking = false;

  void init() {
    sherpa.initBindings();
    _gateVad = Vad(
      modelPath: config.vadModel,
      threshold: config.vadThreshold,
      minSilence: 0.2,
      minSpeech: 0.1,
      numThreads: 1,
    )..init();
    _captureVad = Vad(
      modelPath: config.vadModel,
      threshold: config.vadThreshold,
      minSilence: config.minSilence,
      minSpeech: 0.2,
      maxSpeech: config.maxUtteranceSeconds,
      numThreads: 1,
    )..init();
    _wake = WakeWordDetector(
      encoder: config.kwsEncoder,
      decoder: config.kwsDecoder,
      joiner: config.kwsJoiner,
      tokens: config.kwsTokens,
      keywordsFile: config.keywordsFile,
      keywordsThreshold: config.wakeThreshold,
      keywordsScore: config.wakeScore,
      numThreads: config.numThreads,
    )..init();
    // Whisper is loaded lazily on first capture and can be unloaded when idle.
    _asr = WhisperAsr(
      encoder: config.whisperEncoder,
      decoder: config.whisperDecoder,
      tokens: config.whisperTokens,
      language: config.asrLanguage,
      numThreads: config.numThreads,
    );
    _maxCaptureSamples = (config.maxUtteranceSeconds * kSampleRate).round();
  }

  void onAudio(Float32List samples) {
    if (suppressed) {
      _preroll.addAll(samples);
      return;
    }
    switch (_state) {
      case _State.listeningWake:
        _listen(samples);
      case _State.capturing:
        _capture(samples);
    }
  }

  void _listen(Float32List samples) {
    _preroll.addAll(samples);

    // Keep KWS audio continuous; gate only the expensive decode by VAD.
    _wake.feed(samples);

    _gateVad.acceptWaveform(samples);
    while (_gateVad.hasSegment) {
      _gateVad.popSegment(); // we don't need the gate's segments
    }
    final speaking = _gateVad.isSpeaking;
    if (speaking != _wasSpeaking) {
      _wasSpeaking = speaking;
      toMain.send({'type': 'vad', 'speaking': speaking});
    }
    if (!speaking) return;

    final matched = _wake.poll();
    if (matched != null) {
      toMain.send({'type': 'wake', 'keyword': matched});
      _enterCapturing();
    }
  }

  void _enterCapturing() {
    _state = _State.capturing;
    _capturedSamples = 0;
    _sawSpeech = false;
    _captureVad.reset();
    final pre = _preroll.toList();
    if (pre.isNotEmpty) {
      _captureVad.acceptWaveform(pre);
      _capturedSamples += pre.length;
    }
    _preroll.clear();
  }

  void _capture(Float32List samples) {
    _captureVad.acceptWaveform(samples);
    _capturedSamples += samples.length;
    if (_captureVad.isSpeaking) _sawSpeech = true;

    if (_captureVad.hasSegment) {
      _transcribe(_captureVad.popSegment());
      _backToListening();
      return;
    }

    if (_capturedSamples >= _maxCaptureSamples) {
      _captureVad.flush();
      if (_captureVad.hasSegment) {
        _transcribe(_captureVad.popSegment());
      } else {
        toMain.send({'type': 'empty'});
      }
      _backToListening();
    }
  }

  void _transcribe(Float32List segment) {
    if (segment.isEmpty || !_sawSpeech) {
      toMain.send({'type': 'empty'});
      return;
    }
    try {
      final result = _asr.transcribe(segment);
      if (result.text.trim().isEmpty) {
        toMain.send({'type': 'empty'});
      } else {
        toMain.send({
          'type': 'transcript',
          'text': result.text,
          'language': result.language,
        });
      }
    } catch (e) {
      toMain.send({'type': 'error', 'message': 'asr failed: $e'});
    }
  }

  void _backToListening() {
    _state = _State.listeningWake;
    _captureVad.reset();
    _preroll.clear();
    _wasSpeaking = false;
  }

  void unloadAsr() => _asr.free();

  void dispose() {
    _gateVad.free();
    _captureVad.free();
    _wake.free();
    _asr.free();
  }
}
