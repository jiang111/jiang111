/// flutter_speaker_llm — on-device voice commands for Flutter.
///
/// Wake word → offline Whisper ASR → on-device LLM intent recognition →
/// your command handlers, with spoken feedback when nothing matches.
///
/// See [SynlinkEngine] for the entry point.
library flutter_speaker_llm;

export 'src/engine/synlink_engine.dart' show SynlinkEngine, SetupProgress;
export 'src/engine/synlink_config.dart'
    show SynlinkConfig, PowerMode, WhisperModelSize;

export 'src/commands/command.dart' show Command, CommandType;
export 'src/commands/command_definition.dart' show CommandDefinition;
export 'src/commands/registry.dart' show CommandRegistry, CommandHandler;

export 'src/pipeline/voice_pipeline.dart' show PipelineState;

export 'src/llm/tools.dart' show SynlinkTool, kBuiltInTools;

export 'src/tts/voice_feedback.dart'
    show VoiceFeedbackConfig, TtsEngineType, FeedbackLanguage;

export 'src/download/model_sources.dart' show ModelSources;
export 'src/download/model_asset.dart' show ModelBundle, ModelFile;
export 'src/download/download_manager.dart' show DownloadProgress;
