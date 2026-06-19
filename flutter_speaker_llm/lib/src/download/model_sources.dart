import 'dart:io' show Platform;

import 'package:flutter/foundation.dart' show kIsWeb;

import 'model_asset.dart';

bool get _isDesktop {
  if (kIsWeb) return false;
  return Platform.isWindows || Platform.isMacOS || Platform.isLinux;
}

String _trimSlash(String s) => s.replaceAll(RegExp(r'/+$'), '');

/// Declares where every model file is downloaded from.
///
/// The sherpa-onnx assets (KWS, VAD, Whisper) are fetched by the bundled
/// [DownloadManager]; the LLM is fetched by `flutter_gemma` from [llmUrl].
///
/// The easiest way to point everything at your own CDN is
/// [ModelSources.fromBaseUrl], which expects this layout:
///
/// ```
/// <base>/kws/{encoder,decoder,joiner}.onnx  tokens.txt  keywords.txt
/// <base>/vad/silero_vad.onnx
/// <base>/whisper/{tiny,base}-{encoder,decoder}.int8.onnx  {tiny,base}-tokens.txt
/// <base>/llm/qwen2.5-0.5b-instruct.{task,litertlm}
/// ```
class ModelSources {
  const ModelSources({
    required this.kws,
    required this.vad,
    required this.whisperTiny,
    required this.whisperBase,
    required this.llmTaskUrl,
    required this.llmLitertlmUrl,
    this.tts,
  });

  final ModelBundle kws;
  final ModelBundle vad;
  final ModelBundle whisperTiny;
  final ModelBundle whisperBase;

  /// LLM URL for mobile/web (`.task`, MediaPipe format).
  final String llmTaskUrl;

  /// LLM URL for desktop (`.litertlm`, LiteRT-LM format).
  final String llmLitertlmUrl;

  /// Optional fully-offline sherpa TTS bundle.
  final ModelBundle? tts;

  /// The LLM URL appropriate for the current platform.
  String get llmUrl => _isDesktop ? llmLitertlmUrl : llmTaskUrl;

  /// Builds every URL from a single [base], mirroring the documented layout.
  /// Host the files on your CDN under that layout and you're done.
  factory ModelSources.fromBaseUrl(String base, {bool includeTts = false}) {
    final b = _trimSlash(base);
    String u(String path) => '$b/$path';
    return ModelSources(
      kws: ModelBundle(id: 'kws', dirName: 'kws', files: [
        ModelFile(name: 'encoder.onnx', url: u('kws/encoder.onnx')),
        ModelFile(name: 'decoder.onnx', url: u('kws/decoder.onnx')),
        ModelFile(name: 'joiner.onnx', url: u('kws/joiner.onnx')),
        ModelFile(name: 'tokens.txt', url: u('kws/tokens.txt')),
        ModelFile(name: 'keywords.txt', url: u('kws/keywords.txt')),
      ]),
      vad: ModelBundle(id: 'vad', dirName: 'vad', files: [
        ModelFile(name: 'silero_vad.onnx', url: u('vad/silero_vad.onnx')),
      ]),
      whisperTiny: ModelBundle(id: 'whisper', dirName: 'whisper', files: [
        ModelFile(
            name: 'tiny-encoder.int8.onnx',
            url: u('whisper/tiny-encoder.int8.onnx')),
        ModelFile(
            name: 'tiny-decoder.int8.onnx',
            url: u('whisper/tiny-decoder.int8.onnx')),
        ModelFile(name: 'tiny-tokens.txt', url: u('whisper/tiny-tokens.txt')),
      ]),
      whisperBase: ModelBundle(id: 'whisper', dirName: 'whisper', files: [
        ModelFile(
            name: 'base-encoder.int8.onnx',
            url: u('whisper/base-encoder.int8.onnx')),
        ModelFile(
            name: 'base-decoder.int8.onnx',
            url: u('whisper/base-decoder.int8.onnx')),
        ModelFile(name: 'base-tokens.txt', url: u('whisper/base-tokens.txt')),
      ]),
      llmTaskUrl: u('llm/qwen2.5-0.5b-instruct.task'),
      llmLitertlmUrl: u('llm/qwen2.5-0.5b-instruct.litertlm'),
      tts: includeTts
          ? ModelBundle(id: 'tts', dirName: 'tts', files: [
              ModelFile(name: 'model.onnx', url: u('tts/model.onnx')),
              ModelFile(name: 'tokens.txt', url: u('tts/tokens.txt')),
              ModelFile(name: 'lexicon.txt', url: u('tts/lexicon.txt')),
            ])
          : null,
    );
  }

  /// The official upstream download URLs (HuggingFace / GitHub releases).
  ///
  /// See `docs/MODELS.md` for the authoritative list and notes. For reliable
  /// downloads in all regions, mirror these to your CDN and use
  /// [ModelSources.fromBaseUrl] instead.
  factory ModelSources.defaults() => _officialSources;

  ModelSources copyWith({
    ModelBundle? kws,
    ModelBundle? vad,
    ModelBundle? whisperTiny,
    ModelBundle? whisperBase,
    String? llmTaskUrl,
    String? llmLitertlmUrl,
    ModelBundle? tts,
  }) {
    return ModelSources(
      kws: kws ?? this.kws,
      vad: vad ?? this.vad,
      whisperTiny: whisperTiny ?? this.whisperTiny,
      whisperBase: whisperBase ?? this.whisperBase,
      llmTaskUrl: llmTaskUrl ?? this.llmTaskUrl,
      llmLitertlmUrl: llmLitertlmUrl ?? this.llmLitertlmUrl,
      tts: tts ?? this.tts,
    );
  }
}

// NOTE: These default upstream URLs are filled in once verified against the
// current sherpa-onnx / flutter_gemma releases (see docs/MODELS.md). Most
// integrators should mirror the files to their own CDN and use
// `ModelSources.fromBaseUrl(...)` instead.
final ModelSources _officialSources = ModelSources(
  kws: const ModelBundle(id: 'kws', dirName: 'kws', files: [
    ModelFile(
        name: 'encoder.onnx',
        url:
            'https://huggingface.co/pkufool/sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01/resolve/main/encoder-epoch-12-avg-2-chunk-16-left-64.onnx'),
    ModelFile(
        name: 'decoder.onnx',
        url:
            'https://huggingface.co/pkufool/sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01/resolve/main/decoder-epoch-12-avg-2-chunk-16-left-64.onnx'),
    ModelFile(
        name: 'joiner.onnx',
        url:
            'https://huggingface.co/pkufool/sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01/resolve/main/joiner-epoch-12-avg-2-chunk-16-left-64.onnx'),
    ModelFile(
        name: 'tokens.txt',
        url:
            'https://huggingface.co/pkufool/sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01/resolve/main/tokens.txt'),
    // keywords.txt is generated locally for the "hi synlink" phrase — see
    // docs/MODELS.md. A starter file is bundled as an asset by the example app.
    ModelFile(
        name: 'keywords.txt',
        url:
            'https://huggingface.co/pkufool/sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01/resolve/main/test_wavs/test_keywords.txt'),
  ]),
  vad: const ModelBundle(id: 'vad', dirName: 'vad', files: [
    ModelFile(
        name: 'silero_vad.onnx',
        url:
            'https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx'),
  ]),
  whisperTiny: const ModelBundle(id: 'whisper', dirName: 'whisper', files: [
    ModelFile(
        name: 'tiny-encoder.int8.onnx',
        url:
            'https://huggingface.co/csukuangfj/sherpa-onnx-whisper-tiny/resolve/main/tiny-encoder.int8.onnx'),
    ModelFile(
        name: 'tiny-decoder.int8.onnx',
        url:
            'https://huggingface.co/csukuangfj/sherpa-onnx-whisper-tiny/resolve/main/tiny-decoder.int8.onnx'),
    ModelFile(
        name: 'tiny-tokens.txt',
        url:
            'https://huggingface.co/csukuangfj/sherpa-onnx-whisper-tiny/resolve/main/tiny-tokens.txt'),
  ]),
  whisperBase: const ModelBundle(id: 'whisper', dirName: 'whisper', files: [
    ModelFile(
        name: 'base-encoder.int8.onnx',
        url:
            'https://huggingface.co/csukuangfj/sherpa-onnx-whisper-base/resolve/main/base-encoder.int8.onnx'),
    ModelFile(
        name: 'base-decoder.int8.onnx',
        url:
            'https://huggingface.co/csukuangfj/sherpa-onnx-whisper-base/resolve/main/base-decoder.int8.onnx'),
    ModelFile(
        name: 'base-tokens.txt',
        url:
            'https://huggingface.co/csukuangfj/sherpa-onnx-whisper-base/resolve/main/base-tokens.txt'),
  ]),
  // Qwen2.5-0.5B LLM URLs are resolved by flutter_gemma; confirm the exact
  // .task / .litertlm asset URLs in docs/MODELS.md and set them here or via
  // ModelSources.fromBaseUrl for your CDN.
  llmTaskUrl:
      'https://huggingface.co/litert-community/Qwen2.5-0.5B-Instruct/resolve/main/Qwen2.5-0.5B-Instruct_multi-prefill-seq_q8_ekv1280.task',
  llmLitertlmUrl:
      'https://huggingface.co/litert-community/Qwen2.5-0.5B-Instruct/resolve/main/Qwen2.5-0.5B-Instruct_multi-prefill-seq_q8_ekv1280.litertlm',
);
