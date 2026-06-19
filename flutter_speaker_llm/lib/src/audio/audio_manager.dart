import 'dart:async';
import 'dart:typed_data';

import 'package:record/record.dart';

import 'pcm.dart';

/// Owns the one and only microphone stream and broadcasts 16 kHz mono
/// `Float32List` chunks to every consumer (wake-word spotter, VAD, ASR).
///
/// Opening the microphone exactly once — rather than per pipeline stage — is
/// what avoids "mic busy" errors and audio-session churn on iOS/Android. The
/// pipeline merely chooses which consumer the broadcast frames are routed to.
class AudioManager {
  AudioManager({AudioRecorder? recorder})
      : _recorder = recorder ?? AudioRecorder();

  final AudioRecorder _recorder;
  StreamSubscription<Uint8List>? _sub;
  StreamController<Float32List>? _controller;
  bool _running = false;

  /// Broadcast stream of normalised samples. Multiple listeners are fine.
  Stream<Float32List> get samples =>
      (_controller ??= StreamController<Float32List>.broadcast()).stream;

  bool get isRunning => _running;

  Future<bool> hasPermission() => _recorder.hasPermission();

  /// Starts capturing. Throws [StateError] if microphone permission is denied.
  Future<void> start() async {
    if (_running) return;
    if (!await _recorder.hasPermission()) {
      throw StateError('Microphone permission not granted');
    }
    _controller ??= StreamController<Float32List>.broadcast();

    final stream = await _recorder.startStream(const RecordConfig(
      encoder: AudioEncoder.pcm16bits,
      sampleRate: kSampleRate,
      numChannels: kNumChannels,
      // Low-latency capture with built-in echo cancellation on Android.
      androidConfig: AndroidRecordConfig(
        audioSource: AndroidAudioSource.voiceRecognition,
      ),
      // Coexist with TTS playback on iOS.
      iosConfig: IosRecordConfig(
        categoryOptions: [
          IosAudioCategoryOption.defaultToSpeaker,
          IosAudioCategoryOption.allowBluetooth,
        ],
      ),
    ));

    _running = true;
    _sub = stream.listen(
      (bytes) {
        final ctrl = _controller;
        if (ctrl != null && !ctrl.isClosed && ctrl.hasListener) {
          ctrl.add(pcm16ToFloat32(bytes));
        }
      },
      onError: (Object e, StackTrace st) => _controller?.addError(e, st),
      cancelOnError: false,
    );
  }

  /// Stops capturing but keeps the broadcast stream alive for restart.
  Future<void> stop() async {
    await _sub?.cancel();
    _sub = null;
    if (_running) {
      await _recorder.stop();
      _running = false;
    }
  }

  Future<void> dispose() async {
    await stop();
    await _controller?.close();
    _controller = null;
    await _recorder.dispose();
  }
}
