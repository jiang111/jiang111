import 'dart:math' as math;
import 'dart:typed_data';

/// A fixed-capacity ring buffer of 32-bit float audio samples.
///
/// Used as a short "pre-roll" before the wake-word detector: while the engine
/// is idle we keep the most recent ~300 ms of audio so that, the moment the
/// cheap VAD gate opens, we can replay the onset into the keyword spotter
/// without clipping the start of "hi synlink".
class Float32RingBuffer {
  Float32RingBuffer(this.capacity)
      : assert(capacity > 0),
        _buffer = Float32List(capacity);

  final int capacity;
  final Float32List _buffer;
  int _start = 0;
  int _length = 0;

  int get length => _length;
  bool get isEmpty => _length == 0;
  bool get isFull => _length == capacity;

  /// Appends [samples], overwriting the oldest data once full.
  void addAll(Float32List samples) {
    for (final s in samples) {
      final writeIndex = (_start + _length) % capacity;
      _buffer[writeIndex] = s;
      if (_length < capacity) {
        _length++;
      } else {
        _start = (_start + 1) % capacity;
      }
    }
  }

  /// Returns the buffered samples in chronological order.
  Float32List toList() {
    final out = Float32List(_length);
    for (var i = 0; i < _length; i++) {
      out[i] = _buffer[(_start + i) % capacity];
    }
    return out;
  }

  /// Returns the most recent [count] samples (or fewer if not yet available).
  Float32List takeLast(int count) {
    final n = math.min(count, _length);
    final out = Float32List(n);
    final offset = _length - n;
    for (var i = 0; i < n; i++) {
      out[i] = _buffer[(_start + offset + i) % capacity];
    }
    return out;
  }

  void clear() {
    _start = 0;
    _length = 0;
  }
}
