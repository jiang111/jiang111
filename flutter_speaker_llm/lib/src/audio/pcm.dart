import 'dart:typed_data';

/// Audio format helpers shared across the capture / ASR pipeline.
///
/// The whole pipeline runs at 16 kHz mono, which is what the sherpa-onnx
/// keyword spotter, VAD and Whisper models expect.
const int kSampleRate = 16000;
const int kNumChannels = 1;

/// Converts little-endian signed 16-bit PCM bytes (as produced by the
/// `record` package with `AudioEncoder.pcm16bits`) into normalised
/// `Float32List` samples in the range `[-1.0, 1.0)`.
Float32List pcm16ToFloat32(Uint8List bytes) {
  // Two bytes per sample. Ignore a trailing odd byte if any.
  final sampleCount = bytes.lengthInBytes ~/ 2;
  final view = ByteData.sublistView(bytes);
  final out = Float32List(sampleCount);
  for (var i = 0; i < sampleCount; i++) {
    final s = view.getInt16(i * 2, Endian.little);
    out[i] = s / 32768.0;
  }
  return out;
}

/// Concatenates a list of Float32 chunks into one contiguous buffer.
Float32List concatFloat32(List<Float32List> chunks) {
  var total = 0;
  for (final c in chunks) {
    total += c.length;
  }
  final out = Float32List(total);
  var offset = 0;
  for (final c in chunks) {
    out.setRange(offset, offset + c.length, c);
    offset += c.length;
  }
  return out;
}
