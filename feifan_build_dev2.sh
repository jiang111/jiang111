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
echo "开始构建2套环境的 web:"
flutter build web --release --target ./lib/main_dev2.dart
echo "构建完成"
dart ./bin/patch.dart http://mapptest2.feifan.art
cd build
echo "开始打包 web.tar.gz"
tar -zcf web.tar.gz ./web/
echo 'web.tar.gz 打包完成,文件路径:'
echo $(pwd)/web.tar.gz