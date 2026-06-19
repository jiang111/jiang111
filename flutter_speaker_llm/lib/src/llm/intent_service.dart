import 'dart:convert';

import '../commands/command.dart';
import 'llm_backend.dart';
import 'tools.dart';

/// Turns a transcript into a [Command] using the on-device LLM.
class IntentService {
  IntentService({
    required this.backend,
    required List<SynlinkTool> tools,
  }) : _tools = [...tools];

  final LlmBackend backend;
  final List<SynlinkTool> _tools;

  List<SynlinkTool> get tools => List.unmodifiable(_tools);

  void addTool(SynlinkTool tool) => _tools.add(tool);

  Future<bool> isInstalled() => backend.isInstalled();

  Future<void> ensureInstalled({LlmInstallProgress? onProgress}) =>
      backend.ensureInstalled(onProgress: onProgress);

  Future<void> load() => backend.load();
  Future<void> unload() => backend.unload();

  /// Recognises the intent in [transcript]. [fallbackLanguage] is used when the
  /// model doesn't report one.
  Future<Command> recognize(
    String transcript, {
    String? fallbackLanguage,
  }) async {
    if (transcript.trim().isEmpty) {
      return Command.unknown(transcript: transcript, language: fallbackLanguage);
    }
    final raw = await backend.complete(
      systemPrompt: buildSystemPrompt(_tools),
      userText: transcript,
    );
    return _parse(raw, transcript, fallbackLanguage);
  }

  Command _parse(String raw, String transcript, String? fallbackLanguage) {
    final json = _extractJson(raw);
    if (json == null) {
      return Command.unknown(transcript: transcript, language: fallbackLanguage);
    }

    final name = (json['command'] as String?)?.trim() ?? '';
    final language = (json['language'] as String?)?.trim().toLowerCase();
    final effectiveLang =
        (language != null && language.isNotEmpty) ? language : fallbackLanguage;

    if (name.isEmpty || name == 'none') {
      return Command.unknown(
        transcript: transcript,
        language: effectiveLang,
      );
    }

    final params = <String, dynamic>{};
    final rawParams = json['params'];
    if (rawParams is Map) {
      rawParams.forEach((k, v) => params['$k'] = v);
    }

    return Command(
      name: name,
      params: params,
      transcript: transcript,
      language: effectiveLang,
    );
  }

  /// Pulls the first balanced JSON object out of [raw], tolerating code fences
  /// or stray prose around it.
  Map<String, dynamic>? _extractJson(String raw) {
    final start = raw.indexOf('{');
    if (start < 0) return null;
    var depth = 0;
    var inString = false;
    var escape = false;
    for (var i = start; i < raw.length; i++) {
      final ch = raw[i];
      if (inString) {
        if (escape) {
          escape = false;
        } else if (ch == r'\') {
          escape = true;
        } else if (ch == '"') {
          inString = false;
        }
        continue;
      }
      if (ch == '"') {
        inString = true;
      } else if (ch == '{') {
        depth++;
      } else if (ch == '}') {
        depth--;
        if (depth == 0) {
          final slice = raw.substring(start, i + 1);
          try {
            final decoded = jsonDecode(slice);
            return decoded is Map<String, dynamic> ? decoded : null;
          } catch (_) {
            return null;
          }
        }
      }
    }
    return null;
  }
}
