/// Progress callback for LLM model installation.
typedef LlmInstallProgress = void Function(double fraction);

/// Backend-agnostic interface for the on-device LLM.
///
/// The concrete implementation ([GemmaBackend]) wraps `flutter_gemma`, but
/// keeping [IntentService] behind this interface isolates the version-sensitive
/// plugin calls in one place and makes the LLM swappable / mockable in tests.
abstract class LlmBackend {
  /// Whether the model files are already present on device.
  Future<bool> isInstalled();

  /// Downloads/installs the model if needed.
  Future<void> ensureInstalled({LlmInstallProgress? onProgress});

  /// Loads the model into memory (lazy; safe to call repeatedly).
  Future<void> load();

  /// Frees the model from memory to save RAM.
  Future<void> unload();

  /// Runs one completion: [systemPrompt] sets the rules, [userText] is the
  /// transcribed utterance. Returns the raw model text (expected to be JSON).
  Future<String> complete({
    required String systemPrompt,
    required String userText,
  });

  Future<void> dispose();
}
