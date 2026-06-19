import 'package:meta/meta.dart';

/// A single downloadable file that belongs to a model bundle.
@immutable
class ModelFile {
  const ModelFile({
    required this.name,
    required this.url,
    this.sizeBytes,
    this.sha256,
  });

  /// Local file name to store the file under (e.g. `tiny-encoder.int8.onnx`).
  final String name;

  /// Remote URL to download from. Replace these with your own CDN at runtime
  /// via [ModelSources] overrides.
  final String url;

  /// Optional expected size, used for progress when the server omits
  /// `Content-Length`.
  final int? sizeBytes;

  /// Optional lowercase hex SHA-256 used to verify the download.
  final String? sha256;

  ModelFile copyWith({String? url, int? sizeBytes, String? sha256}) => ModelFile(
        name: name,
        url: url ?? this.url,
        sizeBytes: sizeBytes ?? this.sizeBytes,
        sha256: sha256 ?? this.sha256,
      );
}

/// A group of files that together make up one model (e.g. Whisper needs an
/// encoder, decoder and tokens file).
///
/// The on-device LLM (flutter_gemma) is intentionally NOT modelled here: it
/// manages its own download. This type covers the sherpa-onnx assets
/// (keyword spotter, VAD, Whisper, optional offline TTS).
@immutable
class ModelBundle {
  const ModelBundle({
    required this.id,
    required this.dirName,
    required this.files,
  });

  /// Stable identifier, e.g. `kws`, `vad`, `whisper`, `tts`.
  final String id;

  /// Sub-directory (under the models root) the files are stored in.
  final String dirName;

  final List<ModelFile> files;

  /// Sum of known file sizes, or `null` if any size is unknown.
  int? get totalBytes {
    var sum = 0;
    for (final f in files) {
      if (f.sizeBytes == null) return null;
      sum += f.sizeBytes!;
    }
    return sum;
  }

  ModelBundle copyWith({String? dirName, List<ModelFile>? files}) => ModelBundle(
        id: id,
        dirName: dirName ?? this.dirName,
        files: files ?? this.files,
      );
}
