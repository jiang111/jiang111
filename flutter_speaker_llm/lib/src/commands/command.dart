import 'package:meta/meta.dart';

/// A recognised command, ready to be dispatched to a handler.
///
/// Commands are identified by [name] (the tool name the LLM emitted, matching a
/// [CommandDefinition] or a tool added via `engine.addTool`). The library ships
/// no built-in commands — the integrating app defines its own.
@immutable
class Command {
  const Command({
    required this.name,
    this.params = const {},
    required this.transcript,
    this.language,
    this.confidence,
  });

  /// Builds the "nothing matched" command.
  factory Command.unknown({
    required String transcript,
    String? language,
  }) {
    return Command(name: '', transcript: transcript, language: language);
  }

  /// Tool name the model chose, e.g. `goto_target`. Empty when nothing matched.
  final String name;

  /// Arguments extracted by the model, e.g. `{'target': 'M31'}`.
  final Map<String, dynamic> params;

  /// The ASR transcript that produced this command.
  final String transcript;

  /// Detected language code of [transcript], e.g. `zh`, `en`, `ja`, `es`.
  final String? language;

  /// Optional model confidence in `[0, 1]`, when available.
  final double? confidence;

  /// Whether the model matched no command.
  bool get isUnknown => name.isEmpty;

  /// Convenience accessor for a `target` parameter (e.g. a GOTO target `M31`).
  String? get target => params['target'] as String?;

  Command copyWith({
    String? name,
    Map<String, dynamic>? params,
    String? transcript,
    String? language,
    double? confidence,
  }) {
    return Command(
      name: name ?? this.name,
      params: params ?? this.params,
      transcript: transcript ?? this.transcript,
      language: language ?? this.language,
      confidence: confidence ?? this.confidence,
    );
  }

  @override
  String toString() => 'Command(${name.isEmpty ? '<none>' : name}, '
      'params: $params, lang: $language, transcript: "$transcript")';
}
