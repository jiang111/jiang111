import 'dart:io';

import 'package:audioplayers/audioplayers.dart';
import 'package:path/path.dart' as p;
import 'package:sherpa_onnx/sherpa_onnx.dart' as sherpa;

import 'voice_feedback.dart';

/// Resolved file paths for a sherpa-onnx VITS TTS model (e.g.
/// `vits-melo-tts-zh_en`). Obtain these from the [DownloadManager] after the
/// optional TTS bundle has been downloaded.
class SherpaTtsModelPaths {
  const SherpaTtsModelPaths({
    required this.model,
    required this.tokens,
    this.lexicon = '',
    this.dataDir = '',
    this.dictDir = '',
  });

  final String model;
  final String tokens;
  final String lexicon;
  final String dataDir;
  final String dictDir;
}

/// Fully-offline TTS backed by sherpa-onnx. Optional — only used when
/// [TtsEngineType.sherpaOffline] is selected and the model has been
/// downloaded. Generation + playback run on the calling isolate; for heavy use
/// you may move generation into a worker isolate (see README).
class SherpaTtsEngine implements TtsEngine {
  SherpaTtsEngine({
    required this.paths,
    required this.cacheDir,
    this.speed = 1.0,
    this.speakerId = 0,
  });

  final SherpaTtsModelPaths paths;
  final Directory cacheDir;
  final double speed;
  final int speakerId;

  sherpa.OfflineTts? _tts;
  final AudioPlayer _player = AudioPlayer();
  int _counter = 0;

  @override
  Future<void> init() async {
    if (_tts != null) return;
    sherpa.initBindings();
    final vits = sherpa.OfflineTtsVitsModelConfig(
      model: paths.model,
      tokens: paths.tokens,
      lexicon: paths.lexicon,
      dataDir: paths.dataDir,
      dictDir: paths.dictDir,
    );
    final modelConfig = sherpa.OfflineTtsModelConfig(
      vits: vits,
      numThreads: 1,
      debug: false,
      provider: 'cpu',
    );
    _tts = sherpa.OfflineTts(sherpa.OfflineTtsConfig(model: modelConfig));
    await cacheDir.create(recursive: true);
  }

  @override
  Future<void> speak(String text, {String? language}) async {
    await init();
    final audio = _tts!.generate(text: text, sid: speakerId, speed: speed);
    final file = p.join(cacheDir.path, 'tts_${_counter++}.wav');
    sherpa.writeWave(
      filename: file,
      samples: audio.samples,
      sampleRate: audio.sampleRate,
    );
    await _player.stop();
    await _player.play(DeviceFileSource(file));
    // Resolve only when playback finishes so the caller can resume listening.
    await _player.onPlayerComplete.first;
  }

  @override
  Future<void> stop() => _player.stop();

  @override
  Future<void> dispose() async {
    await _player.dispose();
    _tts?.free();
    _tts = null;
  }
}
