import 'dart:async';

import 'command.dart';

/// A handler invoked when a [Command] of a given [CommandType] is recognised.
typedef CommandHandler = FutureOr<void> Function(Command command);

/// Holds the integrator-supplied handlers and dispatches recognised commands
/// to them.
///
/// The library ships no behaviour for the built-in commands — the host app
/// registers what each one should do:
///
/// ```dart
/// registry.on(CommandType.polarAlign, (c) => mount.startPolarAlign());
/// registry.on(CommandType.gotoTarget, (c) => mount.goto(c.target!));
/// registry.onUnknown((c) => log('not recognised: ${c.transcript}'));
/// ```
class CommandRegistry {
  final Map<CommandType, CommandHandler> _handlers = {};
  final Map<String, CommandHandler> _toolHandlers = {};
  CommandHandler? _unknownHandler;

  /// Registers [handler] for a built-in [type], replacing any previous handler.
  void on(CommandType type, CommandHandler handler) {
    _handlers[type] = handler;
  }

  /// Registers [handler] for a custom tool by its tool name (see
  /// `engine.addTool`). Use this for commands beyond the built-in set.
  void onTool(String toolName, CommandHandler handler) {
    _toolHandlers[toolName] = handler;
  }

  /// Registers a fallback invoked when nothing matched, or when a recognised
  /// command has no registered handler.
  void onUnknown(CommandHandler handler) {
    _unknownHandler = handler;
  }

  /// Removes the handler for [type].
  void remove(CommandType type) => _handlers.remove(type);

  /// Clears all handlers.
  void clear() {
    _handlers.clear();
    _toolHandlers.clear();
    _unknownHandler = null;
  }

  bool hasHandler(CommandType type) => _handlers.containsKey(type);

  /// Dispatches [command] to the matching handler.
  ///
  /// Returns `true` if a concrete handler (built-in or custom tool) ran.
  /// Returns `false` when nothing matched or no handler was registered — the
  /// caller can then decide whether to trigger voice feedback. Any
  /// [onUnknown] fallback is still invoked in the `false` case.
  Future<bool> dispatch(Command command) async {
    final byType = _handlers[command.type];
    if (command.type != CommandType.unknown && byType != null) {
      await byType(command);
      return true;
    }
    final byName = _toolHandlers[command.toolName];
    if (command.toolName.isNotEmpty && byName != null) {
      await byName(command);
      return true;
    }
    if (_unknownHandler != null) {
      await _unknownHandler!(command);
    }
    return false;
  }
}
