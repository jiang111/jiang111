import '../llm/tools.dart';
import 'registry.dart';

/// A fully self-contained command the integrating app configures: what the LLM
/// should recognise (name/description/parameters) AND what to do when it fires
/// (handler) — all in one place.
///
/// Pass a list of these via `SynlinkConfig.commands` to define your own command
/// set without touching the library:
///
/// ```dart
/// SynlinkConfig(
///   commands: [
///     CommandDefinition(
///       name: 'set_temperature',
///       description: 'Set the cooler target temperature in Celsius. 设置制冷温度.',
///       parameters: {
///         'type': 'object',
///         'properties': {'celsius': {'type': 'number'}},
///         'required': ['celsius'],
///       },
///       handler: (c) => cooler.setTarget(c.params['celsius']),
///     ),
///   ],
/// );
/// ```
class CommandDefinition {
  const CommandDefinition({
    required this.name,
    required this.description,
    this.parameters = const {},
    required this.handler,
  });

  /// Tool name the model emits (snake_case), e.g. `set_temperature`.
  final String name;

  /// Natural-language description shown to the model (any language).
  final String description;

  /// JSON-schema object describing parameters (may be empty).
  final Map<String, dynamic> parameters;

  /// Invoked when this command is recognised. The recognised [Command] carries
  /// the extracted `params`, the original `transcript` and detected `language`.
  final CommandHandler handler;

  /// The tool definition exposed to the LLM.
  SynlinkTool toTool() => SynlinkTool(
        name: name,
        description: description,
        parameters: parameters,
      );
}
