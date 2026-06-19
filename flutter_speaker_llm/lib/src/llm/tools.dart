import 'dart:convert';

/// A tool/function the LLM may choose. The built-in ones map to [CommandType];
/// integrators can add their own via `engine.addTool`.
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

/// The six built-in telescope commands.
const List<SynlinkTool> kBuiltInTools = [
  SynlinkTool(
    name: 'polar_align',
    description: 'Start polar alignment of the mount. 对极轴 / 极轴校准.',
  ),
  SynlinkTool(
    name: 'goto_target',
    description:
        'Slew (GOTO) the telescope to a named celestial target. Use for '
        '"goto / go to / 到 / 对准 / 指向 <object>" requests.',
    parameters: {
      'type': 'object',
      'properties': {
        'target': {
          'type': 'string',
          'description': 'Target name, e.g. M31, Jupiter, NGC 7000, 月亮',
        },
      },
      'required': ['target'],
    },
  ),
  SynlinkTool(
    name: 'focus',
    description: 'Run autofocus / focus the camera. 对焦.',
  ),
  SynlinkTool(
    name: 'capture',
    description: 'Start an imaging capture session. 拍摄 / 开始拍摄.',
  ),
  SynlinkTool(
    name: 'single_shot',
    description: 'Take a single exposure / one frame. 拍单张.',
  ),
  SynlinkTool(
    name: 'download_image',
    description: 'Download the most recent image to the device. 下载图片.',
  ),
];

/// Builds the system prompt that turns a multilingual utterance into a strict
/// JSON intent. We rely on structured-output prompting (which Qwen handles
/// well) rather than a vendor-specific function-call API, for robustness
/// across `flutter_gemma` versions.
String buildSystemPrompt(List<SynlinkTool> tools) {
  final toolLines = tools.map((t) {
    final params = t.parameters.isEmpty
        ? 'no parameters'
        : 'parameters JSON-schema: ${jsonEncode(t.parameters)}';
    return '- ${t.name}: ${t.description} ($params)';
  }).join('\n');

  return '''
You are a voice command router for a telescope control app. The user speaks in
any language (Chinese, English, Japanese, Spanish, ...). Map the user's
utterance to exactly ONE of the available tools, extracting any parameters.

Available tools:
$toolLines

Rules:
- Respond with ONLY a single minified JSON object, no prose, no code fences.
- Shape: {"command": <tool name or "none">, "params": <object>, "language": <ISO-639-1 code of the user's utterance>}
- If no tool clearly matches, use "command": "none".
- "language" is the language the user actually spoke, e.g. "zh", "en", "ja", "es".
- Only include parameters defined by the chosen tool.

Examples:
User: 请帮我对极轴
{"command":"polar_align","params":{},"language":"zh"}
User: goto M31
{"command":"goto_target","params":{"target":"M31"},"language":"en"}
User: por favor enfoca la cámara
{"command":"focus","params":{},"language":"es"}
User: 木星に向けて
{"command":"goto_target","params":{"target":"Jupiter"},"language":"ja"}
User: 拍一张
{"command":"single_shot","params":{},"language":"zh"}
User: what's the weather like
{"command":"none","params":{},"language":"en"}
''';
}
