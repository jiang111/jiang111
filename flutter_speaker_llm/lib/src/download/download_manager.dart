import 'dart:async';
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:dio/dio.dart';
import 'package:path/path.dart' as p;

import 'model_asset.dart';

/// Progress for an in-flight bundle download.
class DownloadProgress {
  const DownloadProgress({
    required this.bundleId,
    required this.fileName,
    required this.fileIndex,
    required this.fileCount,
    required this.receivedBytes,
    required this.totalBytes,
  });

  final String bundleId;
  final String fileName;

  /// 0-based index of the file currently downloading.
  final int fileIndex;
  final int fileCount;

  final int receivedBytes;

  /// Total bytes for the current file, or `-1` if unknown.
  final int totalBytes;

  /// Fraction in `[0, 1]` for the current file, or `null` if size unknown.
  double? get fileFraction =>
      totalBytes > 0 ? (receivedBytes / totalBytes).clamp(0.0, 1.0) : null;

  /// Coarse overall fraction across the bundle, assuming equal-sized files.
  double get overallFraction {
    final perFile = fileFraction ?? 0.0;
    return ((fileIndex + perFile) / fileCount).clamp(0.0, 1.0);
  }

  @override
  String toString() =>
      'DownloadProgress($bundleId $fileName ${fileIndex + 1}/$fileCount '
      '${(overallFraction * 100).toStringAsFixed(1)}%)';
}

typedef DownloadProgressCallback = void Function(DownloadProgress progress);

/// Downloads sherpa-onnx model bundles with resume, retry and SHA-256
/// verification.
///
/// Files are streamed to a `.part` file and atomically renamed on success so
/// a partial download is never mistaken for a complete one.
class DownloadManager {
  DownloadManager({Dio? dio})
      : _dio = dio ??
            Dio(BaseOptions(
              connectTimeout: const Duration(seconds: 30),
              receiveTimeout: const Duration(minutes: 10),
            ));

  final Dio _dio;

  /// Whether every file of [bundle] already exists under [baseDir] (and, when
  /// a checksum is provided, matches it).
  Future<bool> isBundleInstalled(ModelBundle bundle, Directory baseDir) async {
    final dir = Directory(p.join(baseDir.path, bundle.dirName));
    if (!await dir.exists()) return false;
    for (final file in bundle.files) {
      final f = File(p.join(dir.path, file.name));
      if (!await f.exists()) return false;
      if (file.sizeBytes != null && await f.length() != file.sizeBytes) {
        return false;
      }
    }
    return true;
  }

  /// Absolute path to a downloaded file, for handing to the model configs.
  String filePath(ModelBundle bundle, Directory baseDir, String fileName) =>
      p.join(baseDir.path, bundle.dirName, fileName);

  /// Downloads any missing files of [bundle] into `baseDir/bundle.dirName`.
  Future<void> downloadBundle(
    ModelBundle bundle,
    Directory baseDir, {
    DownloadProgressCallback? onProgress,
    int maxRetries = 4,
    CancelToken? cancelToken,
  }) async {
    final dir = Directory(p.join(baseDir.path, bundle.dirName));
    await dir.create(recursive: true);

    for (var i = 0; i < bundle.files.length; i++) {
      final file = bundle.files[i];
      final dest = File(p.join(dir.path, file.name));

      if (await _isValid(dest, file)) {
        onProgress?.call(DownloadProgress(
          bundleId: bundle.id,
          fileName: file.name,
          fileIndex: i,
          fileCount: bundle.files.length,
          receivedBytes: await dest.length(),
          totalBytes: file.sizeBytes ?? await dest.length(),
        ));
        continue;
      }

      await _downloadFileWithRetry(
        bundle: bundle,
        file: file,
        dest: dest,
        fileIndex: i,
        fileCount: bundle.files.length,
        onProgress: onProgress,
        maxRetries: maxRetries,
        cancelToken: cancelToken,
      );
    }
  }

  Future<void> _downloadFileWithRetry({
    required ModelBundle bundle,
    required ModelFile file,
    required File dest,
    required int fileIndex,
    required int fileCount,
    required DownloadProgressCallback? onProgress,
    required int maxRetries,
    CancelToken? cancelToken,
  }) async {
    final part = File('${dest.path}.part');
    var attempt = 0;
    while (true) {
      try {
        await _streamToFile(
          url: file.url,
          part: part,
          expectedSize: file.sizeBytes,
          cancelToken: cancelToken,
          onReceived: (received, total) => onProgress?.call(DownloadProgress(
            bundleId: bundle.id,
            fileName: file.name,
            fileIndex: fileIndex,
            fileCount: fileCount,
            receivedBytes: received,
            totalBytes: total,
          )),
        );
        if (file.sha256 != null && !await _matchesSha(part, file.sha256!)) {
          await part.delete();
          throw const DownloadException('checksum mismatch');
        }
        await part.rename(dest.path);
        return;
      } catch (e) {
        if (cancelToken?.isCancelled ?? false) rethrow;
        attempt++;
        if (attempt > maxRetries) {
          throw DownloadException(
              'failed to download ${file.name} after $attempt attempts: $e');
        }
        // Exponential backoff: 2s, 4s, 8s, 16s.
        await Future<void>.delayed(Duration(seconds: 1 << attempt));
      }
    }
  }

  /// Streams [url] into [part], resuming from any bytes already present.
  Future<void> _streamToFile({
    required String url,
    required File part,
    required int? expectedSize,
    required void Function(int received, int total) onReceived,
    CancelToken? cancelToken,
  }) async {
    var existing = await part.exists() ? await part.length() : 0;

    final response = await _dio.get<ResponseBody>(
      url,
      options: Options(
        responseType: ResponseType.stream,
        followRedirects: true,
        headers: existing > 0 ? {'range': 'bytes=$existing-'} : null,
        validateStatus: (s) => s != null && s >= 200 && s < 400,
      ),
      cancelToken: cancelToken,
    );

    final resumed = response.statusCode == 206;
    if (!resumed) {
      // Server ignored the Range header — start over.
      existing = 0;
      if (await part.exists()) await part.delete();
    }

    final contentLength = _contentLength(response.headers);
    final total = contentLength != null
        ? existing + contentLength
        : (expectedSize ?? -1);

    final sink = part.openWrite(mode: FileMode.writeOnlyAppend);
    var received = existing;
    try {
      await for (final chunk in response.data!.stream) {
        sink.add(chunk);
        received += chunk.length;
        onReceived(received, total);
      }
      await sink.flush();
    } finally {
      await sink.close();
    }
  }

  int? _contentLength(Headers headers) {
    final v = headers.value(Headers.contentLengthHeader);
    return v != null ? int.tryParse(v) : null;
  }

  Future<bool> _isValid(File dest, ModelFile file) async {
    if (!await dest.exists()) return false;
    if (file.sizeBytes != null && await dest.length() != file.sizeBytes) {
      return false;
    }
    if (file.sha256 != null && !await _matchesSha(dest, file.sha256!)) {
      return false;
    }
    return true;
  }

  Future<bool> _matchesSha(File f, String expected) async {
    final digest = await sha256.bind(f.openRead()).first;
    return digest.toString().toLowerCase() == expected.toLowerCase();
  }
}

class DownloadException implements Exception {
  const DownloadException(this.message);
  final String message;
  @override
  String toString() => 'DownloadException: $message';
}
