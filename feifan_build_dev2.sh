git config --global http.sslVerify false
git config --global http.postBuffer 1048576000
git config --global https.postBuffer 1048576000


flutter_version=$(curl -s https://raw.githubusercontent.com/jiang111/jiang111/master/flutter.version)
git clone https://github.com/flutter/flutter.git -b $flutter_version
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
flutter build web --release --web-renderer canvaskit --target ./lib/main_dev2.dart
echo "构建完成"
dart ./bin/patch.dart http://mapptest2.feifan.art
