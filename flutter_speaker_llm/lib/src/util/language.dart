/// Maps a short language code (ISO-639-1) to a BCP-47 locale suitable for
/// `flutter_tts` / OS speech engines.
const Map<String, String> kBcp47ByLang = {
  'zh': 'zh-CN',
  'en': 'en-US',
  'ja': 'ja-JP',
  'es': 'es-ES',
  'ko': 'ko-KR',
  'fr': 'fr-FR',
  'de': 'de-DE',
  'ru': 'ru-RU',
  'it': 'it-IT',
  'pt': 'pt-PT',
  'ar': 'ar-SA',
  'hi': 'hi-IN',
  'th': 'th-TH',
  'vi': 'vi-VN',
};

/// Returns a best-effort BCP-47 locale for [lang] (which may already be a
/// locale like `zh-TW`).
String bcp47For(String lang) {
  final lower = lang.toLowerCase();
  if (kBcp47ByLang.containsValue(lang)) return lang;
  if (kBcp47ByLang.containsKey(lower)) return kBcp47ByLang[lower]!;
  final base = lower.split(RegExp('[-_]')).first;
  return kBcp47ByLang[base] ?? lang;
}

/// Very small, dependency-free language guesser used only as a fallback when
/// neither the ASR nor the LLM reports a language. It can reliably separate a
/// few scripts (kana → ja, hangul → ko, Han → zh) but cannot distinguish
/// same-script languages (e.g. en vs es), returning `null` in that case so the
/// caller can fall back to the configured default.
String? detectLanguageHeuristic(String text) {
  var hasHan = false;
  for (final rune in text.runes) {
    if (rune >= 0x3040 && rune <= 0x30FF) return 'ja'; // hiragana/katakana
    if (rune >= 0xAC00 && rune <= 0xD7A3) return 'ko'; // hangul syllables
    if (rune >= 0x4E00 && rune <= 0x9FFF) hasHan = true; // CJK unified
  }
  if (hasHan) return 'zh';
  return null;
}

/// Picks the prompt for [lang] from [prompts], falling back to the base
/// language and then to [fallback].
String resolvePrompt(
  Map<String, String> prompts,
  String? lang, {
  String fallback = 'en',
}) {
  if (lang != null) {
    if (prompts.containsKey(lang)) return prompts[lang]!;
    final base = lang.toLowerCase().split(RegExp('[-_]')).first;
    if (prompts.containsKey(base)) return prompts[base]!;
  }
  return prompts[fallback] ?? prompts.values.first;
}
