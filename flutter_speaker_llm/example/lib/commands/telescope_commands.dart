import 'package:flutter/foundation.dart';
import 'package:flutter_speaker_llm/flutter_speaker_llm.dart';

/// The six telescope commands, defined entirely in the app (the library ships
/// none). Replace the stub handler bodies with real mount/camera calls.
///
/// 对极轴 | GOTO 到目标 | 对焦 | 拍摄 | 拍单张 | 下载图片
List<CommandDefinition> telescopeCommands() => [
      CommandDefinition(
        name: 'polar_align',
        description: 'Start polar alignment of the mount. 对极轴 / 极轴校准.',
        handler: (c) {
          debugPrint('[command] polar align (对极轴)');
          // TODO: mount.startPolarAlign();
        },
      ),
      CommandDefinition(
        name: 'goto_target',
        description: 'Slew (GOTO) the telescope to a named celestial target. '
            'Use for "goto / go to / 到 / 对准 / 指向 <object>".',
        parameters: {
          'type': 'object',
          'properties': {
            'target': {
              'type': 'string',
              'description': 'Target name, e.g. M31, Jupiter, NGC 7000, 月亮',
            },
          },
          'required': ['target'],
        },
        handler: (c) {
          debugPrint('[command] goto -> ${c.target}');
          // TODO: mount.goto(c.target!);
        },
      ),
      CommandDefinition(
        name: 'focus',
        description: 'Run autofocus / focus the camera. 对焦.',
        handler: (c) {
          debugPrint('[command] focus (对焦)');
          // TODO: camera.autoFocus();
        },
      ),
      CommandDefinition(
        name: 'capture',
        description: 'Start an imaging capture session. 拍摄 / 开始拍摄.',
        handler: (c) {
          debugPrint('[command] capture (拍摄)');
          // TODO: camera.startCapture();
        },
      ),
      CommandDefinition(
        name: 'single_shot',
        description: 'Take a single exposure / one frame. 拍单张.',
        handler: (c) {
          debugPrint('[command] single shot (拍单张)');
          // TODO: camera.singleShot();
        },
      ),
      CommandDefinition(
        name: 'download_image',
        description: 'Download the most recent image to the device. 下载图片.',
        handler: (c) {
          debugPrint('[command] download image (下载图片)');
          // TODO: gallery.downloadLatest();
        },
      ),
    ];
