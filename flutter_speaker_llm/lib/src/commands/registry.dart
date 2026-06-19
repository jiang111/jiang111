import 'dart:async';

import 'command.dart';

/// A handler invoked when a [Command] with a given tool name is recognised.
typedef CommandHandler = FutureOr<void> Function(Command command);

/// Holds the integrator-supplied handlers and dispatches recognised commands by
/// tool name.
///
/// ```dart
/// registry.on('polar_align', (c) => mount.startPolarAlign());
/// registry.on('goto_target', (c) => mount.goto(c.target!));
/// registry.onUnknown((c) => log('not recognised: ${c.transcript}'));
/// ```
///
/// You usually don't call [on] directly — pass [CommandDefinition]s via
/// `SynlinkConfig.commands` and the engine registers them for you.
class CommandRegistry {
  final Map<String, CommandHandler> _handlers = {};
  CommandHandler? _unknownHandler;

  /// Registers [handler] for the tool named [name], replacing any previous one.
  void on(String name, CommandHandler handler) {
    _handlers[name] = handler;
  }

  /// Registers a fallback invoked when nothing matched, or when a recognised
  /// command has no registered handler.
  void onUnknown(CommandHandler handler) {
    _unknownHandler = handler;
  }

  void remove(String name) => _handlers.remove(name);

  void clear() {
    _handlers.clear();
    _unknownHandler = null;
  }

  bool hasHandler(String name) => _handlers.containsKey(name);

  /// Dispatches [command] to its handler.
  ///
  /// Returns `true` if a concrete handler ran; `false` when nothing matched or
  /// no handler was registered (the [onUnknown] fallback is still invoked in
  /// that case).
  Future<bool> dispatch(Command command) async {
    final handler = _handlers[command.name];
    if (command.name.isNotEmpty && handler != null) {
      await handler(command);
      return true;
    }
    if (_unknownHandler != null) {
      await _unknownHandler!(command);
    }
    return false;
  }
}
