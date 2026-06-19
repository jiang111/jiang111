import 'dart:convert';

/// A tool/function the LLM may choose. Integrators define these via
/// [CommandDefinition] (`SynlinkConfig.commands`) or `engine.addTool`.
class SynlinkTool {
  const SynlinkTool({
    required this.name,
    required this.description,
    this.parameters = const {},
  });

  /// Stable tool name (snake_case), e.g. `goto_target`.
  final String name;

  /// Natural-language description shown to the model.
  final String description;

  /// JSON-schema object describing parameters (may be empty).
  final Map<String, dynamic> parameters;
}

/// Builds the system prompt that turns a multilingual utterance into a strict
/// JSON intent. We rely on structured-output prompting (which Qwen handles
/// well) rather than a vendor-specific function-call API, for robustness across
/// `flutter_gemma` versions.
String buildSystemPrompt(List<SynlinkTool> tools) {
  final toolLines = tools.map((t) {
    final params = t.parameters.isEmpty
        ? 'no parameters'
        : 'parameters JSON-schema: ${jsonEncode(t.parameters)}';
    return '- ${t.name}: ${t.description} ($params)';
  }).join('\n');

  // A format-only positive example derived from the first configured tool, so
  // we never reference a tool that isn't in the list.
  final positiveExample = tools.isNotEmpty
      ? 'User: <an utterance that means "${tools.first.name}">\n'
          '{"command":"${tools.first.name}","params":{},"language":"en"}\n'
      : '';

  return '''
You are a voice command router. The user speaks in any language (Chinese,
English, Japanese, Spanish, ...). Map the user's utterance to exactly ONE of
the available tools, extracting any parameters.

Available tools:
$toolLines

Rules:
- Respond with ONLY a single minified JSON object, no prose, no code fences.
- Shape: {"command": <tool name or "none">, "params": <object>, "language": <ISO-639-1 code of the user's utterance>}
- Use the EXACT tool names listed above. If none clearly matches, use "none".
- "language" is the language the user actually spoke, e.g. "zh", "en", "ja", "es".
- Only include parameters defined by the chosen tool's schema.

Examples (format only):
${positiveExample}User: 你好，今天天气怎么样
{"command":"none","params":{},"language":"zh"}
User: hello there, how are you
{"command":"none","params":{},"language":"en"}
''';
}
