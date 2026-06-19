import 'package:flutter/material.dart';
import 'package:flutter_speaker_llm/flutter_speaker_llm.dart';
import 'package:permission_handler/permission_handler.dart';

class HomePage extends StatefulWidget {
  const HomePage({super.key, required this.engine});

  final SynlinkEngine engine;

  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> {
  bool _listening = false;
  bool _busy = false;

  Future<void> _toggle() async {
    if (_busy) return;
    setState(() => _busy = true);
    try {
      if (_listening) {
        await widget.engine.stop();
        setState(() => _listening = false);
      } else {
        final status = await Permission.microphone.request();
        if (!status.isGranted) {
          if (mounted) {
            ScaffoldMessenger.of(context).showSnackBar(
              const SnackBar(content: Text('Microphone permission required')),
            );
          }
          return;
        }
        await widget.engine.start();
        setState(() => _listening = true);
      }
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Synlink Voice')),
      body: Padding(
        padding: const EdgeInsets.all(24),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            StreamBuilder<PipelineState>(
              stream: widget.engine.stateStream,
              builder: (_, snap) {
                final state = snap.data ?? PipelineState.idle;
                return Chip(
                  avatar: Icon(_iconFor(state), size: 18),
                  label: Text(state.name),
                );
              },
            ),
            const SizedBox(height: 24),
            Text('Transcript', style: Theme.of(context).textTheme.labelLarge),
            StreamBuilder<String>(
              stream: widget.engine.transcriptStream,
              builder: (_, snap) => Text(
                snap.data ?? '—',
                style: Theme.of(context).textTheme.headlineSmall,
              ),
            ),
            const SizedBox(height: 24),
            Text('Last command', style: Theme.of(context).textTheme.labelLarge),
            StreamBuilder<Command>(
              stream: widget.engine.commandStream,
              builder: (_, snap) {
                final c = snap.data;
                if (c == null) return const Text('—');
                final params = c.params.isEmpty ? '' : ' ${c.params}';
                final name = c.isUnknown ? '(none)' : c.name;
                return Text('$name$params  [${c.language ?? '?'}]');
              },
            ),
            const Spacer(),
            Text(
              _listening
                  ? 'Listening — say "hi synlink", then a command.'
                  : 'Tap the mic to start.',
              textAlign: TextAlign.center,
              style: Theme.of(context).textTheme.bodySmall,
            ),
            const SizedBox(height: 16),
          ],
        ),
      ),
      floatingActionButton: FloatingActionButton.large(
        onPressed: _busy ? null : _toggle,
        child: Icon(_listening ? Icons.stop : Icons.mic),
      ),
    );
  }

  IconData _iconFor(PipelineState state) => switch (state) {
        PipelineState.listening => Icons.hearing,
        PipelineState.capturing => Icons.mic,
        PipelineState.transcribing => Icons.subtitles,
        PipelineState.reasoning => Icons.psychology,
        PipelineState.speaking => Icons.volume_up,
        PipelineState.paused => Icons.pause,
        PipelineState.error => Icons.error_outline,
        PipelineState.idle => Icons.circle_outlined,
      };
}
