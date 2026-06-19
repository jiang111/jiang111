#!/usr/bin/env bash
#
# Generates the example app's native platform folders (android / ios / macos /
# windows) and patches in the microphone + network permissions.
#
# Run once on a machine with the Flutter SDK installed:
#   bash tool/bootstrap.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXAMPLE="$ROOT/example"

command -v flutter >/dev/null 2>&1 || {
  echo "ERROR: flutter not found on PATH. Install the Flutter SDK first." >&2
  exit 1
}

echo "==> Generating platform folders in $EXAMPLE"
cd "$EXAMPLE"
flutter create . \
  --project-name flutter_speaker_llm_example \
  --platforms=android,ios,macos,windows

echo "==> Patching Android permissions"
MANIFEST="android/app/src/main/AndroidManifest.xml"
if [ -f "$MANIFEST" ] && ! grep -q "RECORD_AUDIO" "$MANIFEST"; then
  perl -0pi -e 's/(<manifest[^>]*>)/$1\n    <uses-permission android:name="android.permission.RECORD_AUDIO"\/>\n    <uses-permission android:name="android.permission.INTERNET"\/>/' "$MANIFEST"
fi

echo "==> Patching iOS microphone usage string"
IOS_PLIST="ios/Runner/Info.plist"
if [ -f "$IOS_PLIST" ]; then
  /usr/libexec/PlistBuddy -c "Add :NSMicrophoneUsageDescription string 'Voice commands need the microphone.'" "$IOS_PLIST" 2>/dev/null \
    || /usr/libexec/PlistBuddy -c "Set :NSMicrophoneUsageDescription 'Voice commands need the microphone.'" "$IOS_PLIST"
fi

echo "==> Patching macOS entitlements (audio input + network client)"
for ENT in macos/Runner/DebugProfile.entitlements macos/Runner/Release.entitlements; do
  [ -f "$ENT" ] || continue
  /usr/libexec/PlistBuddy -c "Add :com.apple.security.device.audio-input bool true" "$ENT" 2>/dev/null \
    || /usr/libexec/PlistBuddy -c "Set :com.apple.security.device.audio-input true" "$ENT"
  /usr/libexec/PlistBuddy -c "Add :com.apple.security.network.client bool true" "$ENT" 2>/dev/null \
    || /usr/libexec/PlistBuddy -c "Set :com.apple.security.network.client true" "$ENT"
done

echo "==> flutter pub get"
flutter pub get

cat <<'NOTE'

Done. Platform folders generated and permissions patched.
Desktop LLM (Windows/macOS) also needs flutter_gemma_litertlm — add to
example/pubspec.yaml if you target desktop:
    flutter_gemma_litertlm: ^0.9.0   # verify version

Run it:
    flutter run -d macos      # or windows / an android/ios device
NOTE
