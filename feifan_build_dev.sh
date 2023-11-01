git clone https://github.com/flutter/flutter.git -b stable
cd ./flutter/bin
chmod 774 flutter
chmod 774 dart
./flutter doctor
export PATH=PATH=$PATH:$(pwd)
cd ..
echo flutter.sdk=$(pwd) > emas_config.local.properties
cat emas_config.local.properties > ../android/local.properties
cd ..
echo "开始构建 web:"
flutter build web --release --target ./lib/main_dev.dart
echo "构建完成"
dart ./bin/patch.dart http://mapptest.feifan.art
cd build
tar -zcf web.tar.gz ./web/
echo 'web.tar.gz打包文件路径:'
echo $(pwd)/web.tar.gz