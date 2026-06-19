import 'package:flutter/foundation.dart';
import 'package:flutter_speaker_llm/flutter_speaker_llm.dart';

/// Wires each recognised command to your app's behaviour.
///
/// These are stubs — replace the bodies with real mount/camera calls.
void registerExampleHandlers(SynlinkEngine engine) {
  engine.registry
    ..on(CommandType.polarAlign, (c) {
      debugPrint('[command] polar align (对极轴)');
      // TODO: mount.startPolarAlign();
    })
    ..on(CommandType.gotoTarget, (c) {
      debugPrint('[command] goto -> ${c.target}');
      // TODO: mount.goto(c.target!);
    })
    ..on(CommandType.focus, (c) {
      debugPrint('[command] focus (对焦)');
      // TODO: camera.autoFocus();
    })
    ..on(CommandType.capture, (c) {
      debugPrint('[command] capture (拍摄)');
      // TODO: camera.startCapture();
    })
    ..on(CommandType.singleShot, (c) {
      debugPrint('[command] single shot (拍单张)');
      // TODO: camera.singleShot();
    })
    ..on(CommandType.downloadImage, (c) {
      debugPrint('[command] download image (下载图片)');
      // TODO: gallery.downloadLatest();
    })
    ..onUnknown((c) {
      debugPrint('[command] not recognised: "${c.transcript}"');
    });
}
