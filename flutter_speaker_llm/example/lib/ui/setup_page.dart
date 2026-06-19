import 'package:flutter/material.dart';
import 'package:flutter_speaker_llm/flutter_speaker_llm.dart';

/// First-launch page that downloads the LLM + speech models.
class SetupPage extends StatefulWidget {
  const SetupPage({super.key, required this.engine, required this.onReady});

  final SynlinkEngine engine;
  final VoidCallback onReady;

  @override
  State<SetupPage> createState() => _SetupPageState();
}

class _SetupPageState extends State<SetupPage> {
  bool _downloading = false;
  String _stage = '';
  double _fraction = 0;
  String? _error;

  Future<void> _download() async {
    setState(() {
      _downloading = true;
      _error = null;
    });
    try {
      await widget.engine.ensureModelsReady(onProgress: (p) {
        if (!mounted) return;
        setState(() {
          _stage = p.stage;
          _fraction = p.fraction;
        });
      });
      if (mounted) widget.onReady();
    } catch (e) {
      if (mounted) setState(() => _error = '$e');
    } finally {
      if (mounted) setState(() => _downloading = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Download models')),
      body: Padding(
        padding: const EdgeInsets.all(24),
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            const Text(
              'The voice models (speech recognition + LLM) download once and '
              'run fully on-device afterwards.',
              textAlign: TextAlign.center,
            ),
            const SizedBox(height: 32),
            if (_downloading) ...[
              LinearProgressIndicator(value: _fraction == 0 ? null : _fraction),
              const SizedBox(height: 8),
              Text('Downloading $_stage  ${(_fraction * 100).toStringAsFixed(0)}%',
                  textAlign: TextAlign.center),
            ] else
              FilledButton.icon(
                onPressed: _download,
                icon: const Icon(Icons.download),
                label: const Text('Download'),
              ),
            if (_error != null) ...[
              const SizedBox(height: 16),
              Text(_error!, style: TextStyle(color: Theme.of(context).colorScheme.error)),
            ],
          ],
        ),
      ),
    );
  }
}
