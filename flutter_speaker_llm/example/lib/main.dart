import 'package:flutter/material.dart';
import 'package:flutter_speaker_llm/flutter_speaker_llm.dart';

import 'commands/handlers.dart';
import 'ui/home_page.dart';
import 'ui/setup_page.dart';

void main() => runApp(const ExampleApp());

class ExampleApp extends StatefulWidget {
  const ExampleApp({super.key});

  @override
  State<ExampleApp> createState() => _ExampleAppState();
}

class _ExampleAppState extends State<ExampleApp> {
  late final SynlinkEngine engine;

  @override
  void initState() {
    super.initState();
    engine = SynlinkEngine(
      config: SynlinkConfig(
        // Replace with your CDN:
        // modelSources: ModelSources.fromBaseUrl('https://cdn.example.com/models'),
        modelSources: ModelSources.defaults(),
        whisperModel: WhisperModelSize.base,
        powerMode: PowerMode.balanced,
        voiceFeedback: const VoiceFeedbackConfig(enabled: true),
      ),
    );
    registerExampleHandlers(engine);
  }

  @override
  void dispose() {
    engine.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Synlink Voice',
      theme: ThemeData(colorSchemeSeed: Colors.indigo, useMaterial3: true),
      home: _Gate(engine: engine),
    );
  }
}

/// Shows the download page until all models are present, then the home page.
class _Gate extends StatefulWidget {
  const _Gate({required this.engine});
  final SynlinkEngine engine;

  @override
  State<_Gate> createState() => _GateState();
}

class _GateState extends State<_Gate> {
  bool? _ready;

  @override
  void initState() {
    super.initState();
    widget.engine.isReady().then((v) {
      if (mounted) setState(() => _ready = v);
    });
  }

  @override
  Widget build(BuildContext context) {
    if (_ready == null) {
      return const Scaffold(body: Center(child: CircularProgressIndicator()));
    }
    if (!_ready!) {
      return SetupPage(
        engine: widget.engine,
        onReady: () => setState(() => _ready = true),
      );
    }
    return HomePage(engine: widget.engine);
  }
}
