git clone https://github.com/flutter/flutter.git -b 3.13.9
cd ./flutter/bin
chmod 774 flutter
chmod 774 dart
./flutter doctor
export PATH=PATH=$PATH:$(pwd)
cd ..
echo flutter.sdk=$(pwd) > emas_config.local.properties
cat emas_config.local.properties > ../android/local.properties
cd ..
echo "开始构建1套环境的 web:"
flutter build web --release --web-renderer canvaskit --target ./lib/main_dev.dart
echo "构建完成"
dart ./bin/patch.dart http://mapptest.feifan.art
echo "----------- 完成 --------------"
