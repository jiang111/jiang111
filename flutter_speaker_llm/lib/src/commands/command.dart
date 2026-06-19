import 'package:meta/meta.dart';

/// The set of built-in voice commands the engine knows how to recognise.
///
/// Each value maps 1:1 to a tool/function exposed to the on-device LLM
/// (see `llm/tools.dart`). The integrator attaches a handler for each type
/// it cares about via [CommandRegistry].
enum CommandType {
  /// 对极轴 — polar alignment.
  polarAlign,

  /// GOTO 到特定目标 — slew to a named target. Param: `target`.
  gotoTarget,

  /// 对焦 — autofocus / focus.
  focus,

  /// 拍摄 — start capture / imaging.
  capture,

  /// 拍单张 — take a single exposure.
  singleShot,

  /// 下载图片 — download the latest image.
  downloadImage,

  /// The model produced no confident, valid command. The engine will
  /// usually respond with a spoken "didn't catch that" prompt.
  unknown,
}

/// Maps the raw tool/function name returned by the LLM to a [CommandType].
///
/// The names here must match the tool names declared in `llm/tools.dart`.
CommandType commandTypeFromToolName(String name) {
  switch (name) {
    case 'polar_align':
      return CommandType.polarAlign;
    case 'goto_target':
      return CommandType.gotoTarget;
    case 'focus':
      return CommandType.focus;
    case 'capture':
      return CommandType.capture;
    case 'single_shot':
      return CommandType.singleShot;
    case 'download_image':
      return CommandType.downloadImage;
    default:
      return CommandType.unknown;
  }
}

/// A recognised command, ready to be dispatched to a handler.
@immutable
class Command {
  const Command({
    required this.type,
    required this.toolName,
    this.params = const {},
    required this.transcript,
    this.language,
    this.confidence,
  });

  /// Builds an [unknown] command for the cases where nothing matched.
  factory Command.unknown({
    required String transcript,
    String? language,
    String toolName = '',
  }) {
    return Command(
      type: CommandType.unknown,
      toolName: toolName,
      transcript: transcript,
      language: language,
    );
  }

  /// The resolved command type.
  final CommandType type;

  /// The raw tool/function name the model emitted (useful for custom tools).
  final String toolName;

  /// Arguments extracted by the model, e.g. `{'target': 'M31'}`.
  final Map<String, dynamic> params;

  /// The ASR transcript that produced this command.
  final String transcript;

  /// Detected language code of [transcript], e.g. `zh`, `en`, `ja`, `es`.
  final String? language;

  /// Optional model confidence in `[0, 1]`, when available.
  final double? confidence;

  bool get isUnknown => type == CommandType.unknown;

  /// Convenience accessor for the GOTO target, e.g. `M31`.
  String? get target => params['target'] as String?;

  Command copyWith({
    CommandType? type,
    String? toolName,
    Map<String, dynamic>? params,
    String? transcript,
    String? language,
    double? confidence,
  }) {
    return Command(
      type: type ?? this.type,
      toolName: toolName ?? this.toolName,
      params: params ?? this.params,
      transcript: transcript ?? this.transcript,
      language: language ?? this.language,
      confidence: confidence ?? this.confidence,
    );
  }

  @override
  String toString() =>
      'Command(${type.name}, tool: $toolName, params: $params, '
      'lang: $language, transcript: "$transcript")';
}
