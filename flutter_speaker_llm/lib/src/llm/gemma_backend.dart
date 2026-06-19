import 'package:flutter_gemma/flutter_gemma.dart';

import 'llm_backend.dart';

/// `flutter_gemma` implementation of [LlmBackend] running Qwen2.5-0.5B-Instruct.
///
/// NOTE: `flutter_gemma`'s API has shifted across versions. The calls below
/// target a recent release; verify them against the version you resolve
/// (see docs/VERIFY.md) — in particular `createModel`/`createChat` parameters,
/// the `generateChatResponse` return type, and the model-install method name.
class GemmaBackend implements LlmBackend {
  GemmaBackend({
    required this.modelUrl,
    this.maxTokens = 512,
    this.modelType = ModelType.gemmaIt,
    this.preferredBackend,
  });

  /// Platform-appropriate model URL (`.task` on mobile/web, `.litertlm` on
  /// desktop) — see [ModelSources.llmUrl].
  final String modelUrl;
  final int maxTokens;

  /// Generic instruction-tuned chat type; works for Qwen as well as Gemma.
  final ModelType modelType;
  final PreferredBackend? preferredBackend;

  final FlutterGemma _gemma = FlutterGemmaPlugin.instance;
  InferenceModel? _model;

  @override
  Future<bool> isInstalled() => _gemma.modelManager.isModelInstalled;

  @override
  Future<void> ensureInstalled({LlmInstallProgress? onProgress}) async {
    if (await isInstalled()) return;
    final stream =
        _gemma.modelManager.installModelFromNetworkWithProgress(modelUrl);
    await for (final percent in stream) {
      onProgress?.call(percent / 100.0);
    }
  }

  @override
  Future<void> load() async {
    if (_model != null) return;
    _model = await _gemma.createModel(
      modelType: modelType,
      maxTokens: maxTokens,
      preferredBackend: preferredBackend,
    );
  }

  @override
  Future<void> unload() async {
    await _model?.close();
    _model = null;
  }

  @override
  Future<String> complete({
    required String systemPrompt,
    required String userText,
  }) async {
    await load();
    // Fresh chat each call → no history bleed between commands. Deterministic
    // decoding (temperature 0 / topK 1) for stable JSON.
    final chat = await _model!.createChat(
      temperature: 0.0,
      topK: 1,
      supportsFunctionCalls: false,
    );
    final prompt = '$systemPrompt\n\nUser: $userText\nJSON:';
    await chat.addQueryChunk(Message.text(text: prompt, isUser: true));
    final response = await chat.generateChatResponse();
    return _asText(response);
  }

  /// Tolerates either a `String` or a `ModelResponse`-like object.
  String _asText(Object? response) {
    if (response is String) return response;
    try {
      final dynamic dyn = response;
      final dynamic token = dyn.token ?? dyn.text ?? dyn.message;
      if (token is String) return token;
    } catch (_) {
      // fall through
    }
    return response?.toString() ?? '';
  }

  @override
  Future<void> dispose() async => unload();
}
